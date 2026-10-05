package sdk

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"switchyard/internal/platform/identity"
	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

type SnapshotSource interface {
	Snapshot(context.Context, snapshot.Key) ([]byte, error)
}
type LocalConfig struct {
	ProjectID, EnvironmentID string
	// Bound subscriptions to 100 keys and pin each configured safe default.
	Flags           map[string]evaluation.Value
	RefreshInterval time.Duration
	Now             func() time.Time
}
type slot struct {
	key     snapshot.Key
	safe    evaluation.Value
	current atomic.Pointer[snapshot.Snapshot]
}
type Local struct {
	source   SnapshotSource
	flags    map[string]*slot
	now      func() time.Time
	interval time.Duration
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	closed   atomic.Bool
	refresh  sync.Mutex
}

func NewLocal(source SnapshotSource, cfg LocalConfig) (*Local, error) {
	if source == nil || cfg.ProjectID == "" || cfg.EnvironmentID == "" || len(cfg.Flags) < 1 || len(cfg.Flags) > 100 {
		return nil, ErrInvalid
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = snapshot.RefreshInterval
	}
	if cfg.RefreshInterval < time.Millisecond || cfg.RefreshInterval > snapshot.RefreshInterval {
		return nil, ErrInvalid
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Local{source: source, flags: make(map[string]*slot, len(cfg.Flags)), now: cfg.Now, interval: cfg.RefreshInterval, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	for key, safe := range cfg.Flags {
		if key == "" || safe.Validate(safe.Type) != nil {
			cancel()
			return nil, ErrInvalid
		}
		l.flags[key] = &slot{key: snapshot.Key{ProjectID: cfg.ProjectID, EnvironmentID: cfg.EnvironmentID, FlagKey: key}, safe: evaluation.Value{Type: safe.Type, Data: bytes.Clone(safe.Data)}}
	}
	go l.loop()
	return l, nil
}
func (l *Local) loop() {
	defer close(l.done)
	ticker := time.NewTicker(l.interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			_ = l.Refresh(l.ctx)
		}
	}
}

// Refresh serializes publishers while evaluation remains lock-free. A failed,
// regressing, expired or wrong-scope refresh leaves the original proof untouched.
// Snapshot sources must respect context cancellation, as Remote does.
func (l *Local) Refresh(ctx context.Context) error {
	if l.closed.Load() {
		return ErrClosed
	}
	// A bounded try-lock avoids queued manual refresh goroutines.
	if !l.refresh.TryLock() {
		return ErrUnavailable
	}
	defer l.refresh.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(l.ctx, cancel)
	defer stop()
	ctx, batchCancel := context.WithTimeout(ctx, 5*time.Second)
	defer batchCancel()
	var result error
	for _, entry := range l.flags {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		callCtx, callCancel := context.WithTimeout(ctx, time.Second)
		body, err := l.source.Snapshot(callCtx, entry.key)
		callCancel()
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		next, err := snapshot.Decode(body, entry.key)
		if err != nil || !next.Usable(l.now()) || !evaluation.Equal(next.Safe(), entry.safe) {
			result = errors.Join(result, ErrInvalid)
			continue
		}
		old := entry.current.Load()
		var weight int64
		for _, candidate := range l.flags {
			if candidate == entry {
				continue
			}
			if stored := candidate.current.Load(); stored != nil {
				weight += stored.Weight()
			}
		}
		if weight+next.Weight() > 32<<20 {
			result = errors.Join(result, ErrUnavailable)
			continue
		}
		if next.CanReplace(old) {
			entry.current.Store(next)
		} else if old == nil || !bytes.Equal(next.DefinitionJSON(), old.DefinitionJSON()) || !next.VerifiedAt().Equal(old.VerifiedAt()) {
			result = errors.Join(result, ErrInvalid)
		}
	}
	return result
}
func (l *Local) Evaluate(ctx context.Context, in Input) (Decision, error) {
	if err := validate(in); err != nil {
		return fallback(in, "invalid_input"), err
	}
	if l.closed.Load() {
		return fallback(in, "sdk_closed"), ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return fallback(in, "sdk_canceled"), err
	}
	entry, ok := l.flags[in.Key]
	if !ok {
		return fallback(in, "sdk_unsubscribed"), ErrUnavailable
	}
	if !evaluation.Equal(in.Fallback, entry.safe) {
		return fallback(in, "invalid_input"), ErrInvalid
	}
	current := entry.current.Load()
	if current == nil {
		return fallback(in, "cache_unavailable"), ErrUnavailable
	}
	result, err := current.Evaluate(l.now(), in.UserID, in.Attributes)
	if err != nil {
		return fallback(in, "cache_expired"), ErrUnavailable
	}
	return Decision{Result: result, DecisionID: identity.New("dec_"), Available: true}, nil
}
func (l *Local) Close() error {
	if l.closed.CompareAndSwap(false, true) {
		l.cancel()
	}
	<-l.done
	// Wait for a caller-owned refresh to release its context-bound source call too.
	l.refresh.Lock()
	l.refresh.Unlock()
	return nil
}
