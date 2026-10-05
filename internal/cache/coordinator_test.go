package cache

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

type testClock struct {
	base  time.Time
	nanos atomic.Int64
}

func (c *testClock) now() time.Time          { return c.base.Add(time.Duration(c.nanos.Load())) }
func (c *testClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }
func clock() *testClock                      { return &testClock{base: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)} }
func key(name string) snapshot.Key {
	return snapshot.Key{ProjectID: "project", EnvironmentID: "dev", FlagKey: name}
}
func def(k snapshot.Key, rev int64, killed bool) evaluation.Definition {
	v := evaluation.Value{Type: "boolean", Data: json.RawMessage("false")}
	return evaluation.Definition{ProjectID: k.ProjectID, EnvironmentID: k.EnvironmentID, FlagID: "flag_" + k.FlagKey,
		Key: k.FlagKey, Type: "boolean", Revision: rev, Default: evaluation.Value{Type: "boolean", Data: json.RawMessage("true")}, Safe: v, Killed: killed}
}
func options(c *testClock) Options { o := Defaults(); o.Now = c.now; o.PollInterval = 0; return o }
func coordinator(t *testing.T, source Source, store Store, o Options) *Coordinator {
	t.Helper()
	c, err := NewCoordinator(context.Background(), source, store, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// Observe a real worker transition with a bounded deadline; advance no domain
// clock and use no fixed sleeps to make a race pass.
func observed(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("worker transition was not observed")
		}
		runtime.Gosched()
	}
}

type memoryStore struct {
	mu        sync.Mutex
	value     *snapshot.Snapshot
	reads     int
	revisions []int64
}

func (s *memoryStore) Get(ctx context.Context, k snapshot.Key) (*snapshot.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.value == nil {
		return nil, ErrMiss
	}
	return s.value, nil
}
func (s *memoryStore) Put(ctx context.Context, v *snapshot.Snapshot) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revisions = append(s.revisions, v.Revision())
	if v.CanReplace(s.value) {
		s.value = v
		return true, nil
	}
	return false, nil
}

func TestMissesShareRefreshAndCancellationDoesNotCancelOtherReaders(t *testing.T) {
	clock := clock()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	c := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return def(k, 1, false), nil
		case <-ctx.Done():
			return evaluation.Definition{}, ctx.Err()
		}
	}, nil, options(clock))
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := c.Get(ctx, key("listing")); first <- err }()
	<-started
	var group sync.WaitGroup
	for i := 0; i < 100; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			value, err := c.Get(context.Background(), key("listing"))
			if err != nil || value.Revision() != 1 {
				t.Errorf("shared read=%v %v", value, err)
			}
		}()
	}
	observed(t, func() bool { return c.Stats().Coalesced >= 100 })
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(release)
	group.Wait()
	if calls.Load() != 1 || c.Stats().SourceReads != 1 {
		t.Fatal("miss storm reached source repeatedly")
	}
}

func TestRefreshWorkersQueueAdmissionAndWeightAreBounded(t *testing.T) {
	clock := clock()
	release := make(chan struct{})
	var active, peak atomic.Int32
	o := options(clock)
	o.Workers = 2
	o.QueueSize = 4
	o.MaxEntries = 4
	c := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if old >= n || peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-release:
			return def(k, 1, false), nil
		case <-ctx.Done():
			return evaluation.Definition{}, ctx.Err()
		}
	}, nil, o)
	var group sync.WaitGroup
	for _, name := range []string{"a", "b", "c", "d"} {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			_, err := c.Get(context.Background(), key(name))
			if err != nil {
				t.Error(err)
			}
		}(name)
	}
	observed(t, func() bool { return c.Stats().Entries == 4 && active.Load() == 2 })
	if _, err := c.Get(context.Background(), key("exhausted")); !errors.Is(err, ErrBusy) {
		t.Fatalf("admission=%v", err)
	}
	if peak.Load() > 2 {
		t.Fatal("unbounded source concurrency")
	}
	close(release)
	group.Wait()
	clock.advance(time.Second)
	if _, err := c.Get(context.Background(), key("new_flag")); err != nil {
		t.Fatal(err)
	}
	if c.Stats().Entries > 4 || c.Stats().Evictions == 0 {
		t.Fatal("LRU entry bound not enforced")
	}
	o = options(clock)
	o.MaxWeight = 1
	budget := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) { return def(k, 1, false), nil }, nil, o)
	if _, err := budget.Get(context.Background(), key("large")); !errors.Is(err, ErrBusy) {
		t.Fatalf("payload admission=%v", err)
	}
	if budget.Stats().Weight > 1 {
		t.Fatal("payload budget exceeded")
	}
}

func TestOutageExpiresIntoDeclaredSafeFallbackWithoutRenewal(t *testing.T) {
	clock := clock()
	var failing atomic.Bool
	c := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		if failing.Load() {
			return evaluation.Definition{}, ErrUnavailable
		}
		return def(k, 1, false), nil
	}, nil, options(clock))
	value, err := c.Get(context.Background(), key("listing"))
	if err != nil {
		t.Fatal(err)
	}
	proof := value.VerifiedAt()
	failing.Store(true)
	clock.advance(3 * time.Second)
	value, err = c.Get(context.Background(), key("listing"))
	if err != nil || value.VerifiedAt() != proof {
		t.Fatal("last-known-good lost")
	}
	observed(t, func() bool { return c.Stats().SourceFailures == 1 })
	clock.advance(27 * time.Second)
	fallback := evaluation.Value{Type: "boolean", Data: json.RawMessage("false")}
	result, err := c.Evaluate(context.Background(), key("listing"), "user", nil, fallback)
	if !errors.Is(err, ErrUnavailable) || result.Reason != "cache_expired" || string(result.Value.Data) != "false" || result.RunID != "" {
		t.Fatalf("expired=%+v %v", result, err)
	}
	fallback.Data = json.RawMessage("true")
	if _, err := c.Evaluate(context.Background(), key("listing"), "user", nil, fallback); !errors.Is(err, ErrInvalidFallback) {
		t.Fatalf("unsafe known fallback=%v", err)
	}
	// Even an injected wall clock moving backwards cannot revive a proof this
	// coordinator already expired. Production also retains a monotonic deadline.
	clock.advance(-10 * time.Second)
	fallback.Data = json.RawMessage("false")
	result, err = c.Evaluate(context.Background(), key("listing"), "user", nil, fallback)
	if err == nil || string(result.Value.Data) != "false" {
		t.Fatal("clock rollback revived an expired enabled flag")
	}
	clock.advance(10 * time.Second)
	clock.advance(2 * time.Second)
	failing.Store(false)
	value, err = c.Get(context.Background(), key("listing"))
	if err != nil || value.VerifiedAt() != clock.now() {
		t.Fatalf("repair=%v %v", value, err)
	}
}

func TestInvalidationDiscardsReadStartedBeforeDisable(t *testing.T) {
	clock := clock()
	started := make(chan struct{})
	release := make(chan struct{})
	var reads atomic.Int32
	store := &memoryStore{}
	c := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		if reads.Add(1) == 1 {
			close(started)
			select {
			case <-release:
				return def(k, 1, false), nil
			case <-ctx.Done():
				return evaluation.Definition{}, ctx.Err()
			}
		}
		return def(k, 2, true), nil
	}, store, options(clock))
	done := make(chan *snapshot.Snapshot, 1)
	go func() {
		v, err := c.Get(context.Background(), key("listing"))
		if err != nil {
			t.Error(err)
		}
		done <- v
	}()
	<-started
	c.Invalidate(key("listing"))
	close(release)
	v := <-done
	if v == nil || v.Revision() != 2 {
		t.Fatalf("pre-disable read served: %v", v)
	}
	result, err := v.Evaluate(clock.now(), "user", nil)
	if err != nil || result.Reason != "kill_switch" {
		t.Fatal("disable lost")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.revisions) != 1 || store.revisions[0] != 2 {
		t.Fatalf("old read published: %v", store.revisions)
	}
}

func TestRedisProofAndNegativeCacheDoNotBecomeAuthoritative(t *testing.T) {
	clock := clock()
	old, err := snapshot.New(def(key("listing"), 1, false), clock.now())
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{value: old}
	var reads atomic.Int32
	c := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		reads.Add(1)
		return evaluation.Definition{}, ErrUnavailable
	}, store, options(clock))
	if _, err := c.Get(context.Background(), key("listing")); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 0 || c.Stats().RedisHits != 1 {
		t.Fatal("fresh Redis proof was not reused")
	}
	clock.advance(3 * time.Second)
	c.Poll()
	observed(t, func() bool { return c.Stats().SourceFailures == 1 })
	clock.advance(27 * time.Second)
	if _, err := c.Get(context.Background(), key("listing")); !errors.Is(err, ErrUnavailable) {
		t.Fatal("Redis extended proof")
	}
	if store.reads != 1 {
		t.Fatal("stale refresh kept polling Redis instead of authority")
	}
	var missing atomic.Int32
	negative := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		missing.Add(1)
		return evaluation.Definition{}, ErrNotFound
	}, nil, options(clock))
	for i := 0; i < 10; i++ {
		_, err := negative.Get(context.Background(), key("missing"))
		if !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	if missing.Load() != 1 {
		t.Fatal("negative misses not coalesced")
	}
	clock.advance(2 * time.Second)
	negative.Poll()
	observed(t, func() bool { return missing.Load() == 2 })
}

func TestShutdownCancelsRefreshAndWakesQueuedReaders(t *testing.T) {
	clock := clock()
	started := make(chan struct{})
	o := options(clock)
	o.Workers = 1
	c := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		close(started)
		<-ctx.Done()
		return evaluation.Definition{}, ctx.Err()
	}, nil, o)
	done := make(chan error, 1)
	go func() { _, err := c.Get(context.Background(), key("listing")); done <- err }()
	<-started
	c.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatalf("shutdown=%v", err)
	}
	if _, err := c.Get(context.Background(), key("listing")); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestFullQueueRejectsAdditionalMissWithoutStartingAnotherRead(t *testing.T) {
	clock := clock()
	o := options(clock)
	o.Workers, o.QueueSize, o.MaxEntries = 1, 1, 8
	started, release := make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	c := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		if reads.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return def(k, 1, false), nil
		case <-ctx.Done():
			return evaluation.Definition{}, ctx.Err()
		}
	}, nil, o)
	var group sync.WaitGroup
	for _, name := range []string{"active", "queued"} {
		group.Add(1)
		go func(name string) {
			defer group.Done()
			if _, err := c.Get(context.Background(), key(name)); err != nil {
				t.Error(err)
			}
		}(name)
		if name == "active" {
			<-started
		}
	}
	observed(t, func() bool { return len(c.queue) == 1 })
	if _, err := c.Get(context.Background(), key("rejected")); !errors.Is(err, ErrBusy) {
		t.Fatalf("queue capacity=%v", err)
	}
	if reads.Load() != 1 || c.Stats().Backpressure == 0 {
		t.Fatal("full queue escaped its source/admission bound")
	}
	close(release)
	group.Wait()
}

func TestAuthoritativeQueryTimeConsumesFreshness(t *testing.T) {
	clock := clock()
	proof := clock.now()
	c := coordinator(t, func(ctx context.Context, k snapshot.Key) (evaluation.Definition, error) {
		clock.advance(5 * time.Second)
		return def(k, 1, false), nil
	}, nil, options(clock))
	v, err := c.Get(context.Background(), key("listing"))
	if err != nil || v.VerifiedAt() != proof || v.Remaining(clock.now()) != 25*time.Second {
		t.Fatalf("query renewed freshness: %v %v", v, err)
	}
}
