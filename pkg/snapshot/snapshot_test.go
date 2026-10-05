package snapshot_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

func definition() evaluation.Definition {
	v := evaluation.Value{Type: "boolean", Data: json.RawMessage("false")}
	return evaluation.Definition{ProjectID: "project", EnvironmentID: "environment", FlagID: "flag",
		Key: "listing", Type: "boolean", Revision: 1, Default: v, Safe: v}
}
func must(t *testing.T, d evaluation.Definition, at time.Time) *snapshot.Snapshot {
	t.Helper()
	s, err := snapshot.New(d, at)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFreshnessSurvivesCacheRelayAndProcessRestart(t *testing.T) {
	at := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	s := must(t, definition(), at)
	body, _ := s.MarshalJSON()
	// Three intermediaries decode the same snapshot. None grants a new window.
	for i := 0; i < 3; i++ {
		var err error
		s, err = snapshot.Decode(body, s.Key())
		if err != nil {
			t.Fatal(err)
		}
		body, _ = s.MarshalJSON()
	}
	if s.VerifiedAt() != at || !s.Usable(at.Add(30*time.Second-time.Nanosecond)) ||
		s.Usable(at.Add(30*time.Second)) || s.Usable(at.Add(-time.Nanosecond)) {
		t.Fatal("cache relay, expiry boundary or future time renewed freshness")
	}
	if s.NeedsRefresh(at.Add(2*time.Second-time.Nanosecond)) || !s.NeedsRefresh(at.Add(2*time.Second)) {
		t.Fatal("incorrect authoritative refresh boundary")
	}
	if _, err := s.Evaluate(at.Add(30*time.Second), "user", nil); !errors.Is(err, snapshot.ErrExpired) {
		t.Fatalf("expired evaluation=%v", err)
	}
}

func TestScopeVersionAndMalformedWireRejected(t *testing.T) {
	s := must(t, definition(), time.Now())
	body, _ := s.MarshalJSON()
	other := s.Key()
	other.EnvironmentID = "another"
	if _, err := snapshot.Decode(body, other); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatal("scope escaped")
	}
	for _, invalid := range [][]byte{
		bytes.Replace(body, []byte(`"version":1`), []byte(`"version":2`), 1),
		bytes.Replace(body, []byte(`"version":1`), []byte(`"unexpected":1,"version":1`), 1),
		append(bytes.Clone(body), []byte(` {}`)...), []byte(`null`),
		[]byte(strings.Repeat(" ", snapshot.MaxBytes+1)),
		bytes.Replace(body, []byte(`"revision":1`), []byte(`"revision":0`), 1),
	} {
		if _, err := snapshot.Decode(invalid, s.Key()); !errors.Is(err, snapshot.ErrInvalid) {
			t.Fatalf("invalid wire accepted: %v", err)
		}
	}
	if _, err := snapshot.New(definition(), time.Time{}); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatal("zero proof time accepted")
	}
}

func TestOlderWriterCannotRestoreEnabledConfiguration(t *testing.T) {
	at := time.Now().UTC()
	enabled := definition()
	enabled.Default.Data = json.RawMessage("true")
	old := must(t, enabled, at)
	killed := enabled
	killed.Revision = 2
	killed.Killed = true
	latest := must(t, killed, at.Add(time.Second))
	delayed := must(t, enabled, at.Add(10*time.Second))
	if !latest.CanReplace(old) || delayed.CanReplace(latest) {
		t.Fatal("revision order regressed kill switch")
	}
	conflict := killed
	conflict.Killed = false
	if must(t, conflict, at.Add(2*time.Second)).CanReplace(latest) {
		t.Fatal("same-revision mutation accepted")
	}
	if !must(t, killed, at.Add(2*time.Second)).CanReplace(latest) {
		t.Fatal("authoritative re-verification rejected")
	}
	result, err := latest.Evaluate(at.Add(time.Second), "user", nil)
	if err != nil || result.Reason != "kill_switch" || string(result.Value.Data) != "false" {
		t.Fatalf("kill=%+v %v", result, err)
	}
}

func TestSnapshotOwnsInputAndReturnedBytesDuringConcurrentReads(t *testing.T) {
	at := time.Now().UTC()
	d := definition()
	s := must(t, d, at)
	d.Default.Data[0] = 't'
	body, _ := s.MarshalJSON()
	body[0] = '!'
	safe := s.Safe()
	safe.Data[0] = 't'
	s.DefinitionJSON()[0] = '!'
	var group sync.WaitGroup
	for i := 0; i < 16; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 100; j++ {
				v, err := s.Evaluate(at, "user", nil)
				if err != nil || string(v.Value.Data) != "false" {
					t.Errorf("shared input changed: %+v %v", v, err)
					return
				}
				v.Value.Data[0] = 't'
			}
		}()
	}
	group.Wait()
	if string(s.Safe().Data) != "false" {
		t.Fatal("safe value alias")
	}
}
