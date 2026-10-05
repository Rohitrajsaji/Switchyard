package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switchyard/pkg/evaluation"
	"switchyard/pkg/sdk"
	"switchyard/pkg/snapshot"
)

type sourceFunc func(context.Context, snapshot.Key) ([]byte, error)

func (f sourceFunc) Snapshot(ctx context.Context, key snapshot.Key) ([]byte, error) {
	return f(ctx, key)
}
func boolValue(b bool) evaluation.Value {
	raw := json.RawMessage("false")
	if b {
		raw = json.RawMessage("true")
	}
	return evaluation.Value{Type: "boolean", Data: raw}
}
func wire(t testing.TB, revision int64, proof time.Time, enabled bool) []byte {
	t.Helper()
	s, err := snapshot.New(evaluation.Definition{ProjectID: "project", EnvironmentID: "dev", FlagID: "flag", Key: "listing", Revision: revision, Type: "boolean", Default: boolValue(enabled), Safe: boolValue(false)}, proof)
	if err != nil {
		t.Fatal(err)
	}
	body, err := s.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return body
}
func TestLocalAtomicRefreshStaleProofAndOwnedDefaults(t *testing.T) {
	proof := time.Now()
	var elapsed atomic.Int64
	var response atomic.Pointer[[]byte]
	first := wire(t, 1, proof, true)
	response.Store(&first)
	source := sourceFunc(func(context.Context, snapshot.Key) ([]byte, error) { return *response.Load(), nil })
	safe := boolValue(false)
	local, err := sdk.NewLocal(source, sdk.LocalConfig{ProjectID: "project", EnvironmentID: "dev", Flags: map[string]evaluation.Value{"listing": safe}, Now: func() time.Time { return proof.Add(time.Duration(elapsed.Load())) }})
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	safe.Data[0] = 'x' // Construction owns defaults, so caller mutation cannot change it.
	if err := local.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	var failed atomic.Bool
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 500 {
				_, d, err := sdk.Boolean(context.Background(), local, "listing", "user", nil, false)
				if err != nil || d.Revision < 1 || d.Revision > 2 || !d.Available {
					failed.Store(true)
				}
				if len(d.Value.Data) > 0 {
					d.Value.Data[0] = 'x'
				}
			}
		}()
	}
	next := wire(t, 2, proof, false)
	response.Store(&next)
	if err := local.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	readers.Wait()
	if failed.Load() {
		t.Fatal("partial state during atomic refresh")
	}
	response.Store(&first)
	if err := local.Refresh(context.Background()); !errors.Is(err, sdk.ErrInvalid) {
		t.Fatal("regressing revision admitted", err)
	}
	_, d, err := sdk.Boolean(context.Background(), local, "listing", "user", nil, false)
	if err != nil || d.Revision != 2 || string(d.Value.Data) != "false" {
		t.Fatal("owned result or revision changed", d, err)
	}
	elapsed.Store(int64(29 * time.Second))
	if _, _, err := sdk.Boolean(context.Background(), local, "listing", "user", nil, false); err != nil {
		t.Fatal("last-known-good expired early", err)
	}
	// A disconnected intermediary keeps returning the same proof, even on success.
	response.Store(&next)
	if err := local.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	elapsed.Store(int64(30 * time.Second))
	enabled, d, err := sdk.Boolean(context.Background(), local, "listing", "user", nil, false)
	if enabled || !errors.Is(err, sdk.ErrUnavailable) || d.Reason != "cache_expired" || d.DecisionID != "" || d.Revision != 0 {
		t.Fatal("stale intermediary renewed freshness", d, err)
	}
	if err := local.Refresh(context.Background()); !errors.Is(err, sdk.ErrInvalid) {
		t.Fatal("expired proof installed", err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if _, d, err := sdk.Boolean(context.Background(), local, "listing", "user", nil, false); !errors.Is(err, sdk.ErrClosed) || d.Reason != "sdk_closed" {
		t.Fatal("closed client evaluated", d, err)
	}
}
func TestCloseCancelsRefreshAndDoesNotInventDecision(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	source := sourceFunc(func(ctx context.Context, _ snapshot.Key) ([]byte, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	})
	local, err := sdk.NewLocal(source, sdk.LocalConfig{ProjectID: "project", EnvironmentID: "dev", Flags: map[string]evaluation.Value{"listing": boolValue(false)}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- local.Refresh(context.Background()) }()
	<-entered
	closed := make(chan struct{})
	go func() { _ = local.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not cancel refresh")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("refresh survived close", err)
	}
}
func BenchmarkLocalEvaluate(b *testing.B) {
	proof := time.Now()
	body := wire(b, 1, proof, true)
	local, err := sdk.NewLocal(sourceFunc(func(context.Context, snapshot.Key) ([]byte, error) { return body, nil }), sdk.LocalConfig{ProjectID: "project", EnvironmentID: "dev", Flags: map[string]evaluation.Value{"listing": boolValue(false)}, Now: func() time.Time { return proof }})
	if err != nil {
		b.Fatal(err)
	}
	defer local.Close()
	if err := local.Refresh(context.Background()); err != nil {
		b.Fatal(err)
	}
	input := sdk.Input{Key: "listing", UserID: "user", Fallback: boolValue(false)}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := local.Evaluate(context.Background(), input); err != nil {
			b.Fatal(err)
		}
	}
}
