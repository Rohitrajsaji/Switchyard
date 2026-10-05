package cache

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"sync"
	"time"

	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

var ErrNotFound = errors.New("authoritative flag not found")
var ErrUnavailable = errors.New("configuration unavailable")
var ErrBusy = errors.New("snapshot refresh capacity exhausted")
var ErrClosed = errors.New("snapshot coordinator closed")
var ErrInvalidFallback = errors.New("invalid evaluation fallback or context")
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type Source func(context.Context, snapshot.Key) (evaluation.Definition, error)
type Store interface {
	Get(context.Context, snapshot.Key) (*snapshot.Snapshot, error)
	Put(context.Context, *snapshot.Snapshot) (bool, error)
}
type Options struct {
	MaxEntries    int
	MaxWeight     int64
	Workers       int
	QueueSize     int
	SourceTimeout time.Duration
	StoreTimeout  time.Duration
	PollInterval  time.Duration // Zero permits deterministic explicit Poll tests.
	Now           func() time.Time
}

func Defaults() Options {
	return Options{MaxEntries: 256, MaxWeight: 32 << 20, Workers: 4, QueueSize: 64,
		SourceTimeout: time.Second, StoreTimeout: 300 * time.Millisecond, PollInterval: 250 * time.Millisecond, Now: time.Now}
}

type Stats struct {
	MemoryHits, RedisHits, SourceReads, SourceFailures, StoreFailures uint64
	MemoryMisses, StaleHits                                           uint64
	Coalesced, Backpressure, Evictions, Regressions, Expired          uint64
	Entries                                                           int
	Weight                                                            int64
	// OldestVerificationAge is the age of the stalest snapshot read within the last thirty
	// seconds (idle entries are excluded so they cannot look like violations). It is
	// observational; expiry decisions never use it.
	OldestVerificationAge time.Duration
}
type entry struct {
	value   *snapshot.Snapshot
	expires time.Time // Local monotonic deadline; reads cannot renew it.
	access  time.Time
	nextTry time.Time
	pending chan struct{}
	epoch   uint64
	force   bool
	expired bool
	err     error
}
type job struct {
	key   snapshot.Key
	entry *entry
	epoch uint64
	cold  bool
}
type Coordinator struct {
	mu      sync.Mutex
	entries map[snapshot.Key]*entry
	weight  int64
	stats   Stats
	source  Source
	store   Store
	options Options
	queue   chan job
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closed  bool
}

func NewCoordinator(parent context.Context, source Source, store Store, o Options) (*Coordinator, error) {
	if parent == nil || source == nil || o.Now == nil || o.MaxEntries < 1 || o.MaxWeight < 1 ||
		o.Workers < 1 || o.Workers > 16 || o.QueueSize < 1 || o.SourceTimeout <= 0 || o.StoreTimeout <= 0 || o.PollInterval < 0 {
		return nil, errors.New("invalid snapshot coordinator options")
	}
	ctx, cancel := context.WithCancel(parent)
	c := &Coordinator{entries: make(map[snapshot.Key]*entry), source: source, store: store, options: o,
		queue: make(chan job, o.QueueSize), ctx: ctx, cancel: cancel}
	for i := 0; i < o.Workers; i++ {
		c.wg.Add(1)
		go c.worker()
	}
	if o.PollInterval > 0 {
		c.wg.Add(1)
		go c.poller()
	}
	return c, nil
}
func validKey(k snapshot.Key) bool {
	return len(k.ProjectID) > 0 && len(k.ProjectID) <= 128 && len(k.EnvironmentID) > 0 && len(k.EnvironmentID) <= 128 && keyPattern.MatchString(k.FlagKey)
}
func (c *Coordinator) Close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
	// Workers may exit with bounded jobs still queued; wake every waiting reader.
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if e.pending != nil {
			close(e.pending)
			e.pending = nil
		}
		e.err = ErrClosed
	}
}
func (c *Coordinator) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Entries = len(c.entries)
	s.Weight = c.weight
	now := c.options.Now()
	for _, e := range c.entries {
		if e.value != nil && now.Sub(e.access) <= 30*time.Second {
			if age := now.Sub(e.value.VerifiedAt()); age > s.OldestVerificationAge {
				s.OldestVerificationAge = age
			}
		}
	}
	return s
}
func usable(e *entry, now time.Time) bool {
	if e.value == nil || e.force || errors.Is(e.err, ErrNotFound) {
		return false
	}
	if !now.Before(e.expires) || !e.value.Usable(now) {
		e.expired = true
	}
	return !e.expired
}

// evict removes only idle entries; in-flight work retains its revision fence.
func (c *Coordinator) evict(except *entry) bool {
	var oldest snapshot.Key
	var candidate *entry
	for k, e := range c.entries {
		if e != except && e.pending == nil && (candidate == nil || e.access.Before(candidate.access)) {
			oldest = k
			candidate = e
		}
	}
	if candidate == nil {
		return false
	}
	if candidate.value != nil {
		c.weight -= candidate.value.Weight()
	}
	delete(c.entries, oldest)
	c.stats.Evictions++
	return true
}
func (c *Coordinator) enqueue(k snapshot.Key, e *entry, now time.Time) bool {
	if e.pending != nil {
		return true
	}
	done := make(chan struct{})
	j := job{k, e, e.epoch, e.value == nil && !e.force}
	select {
	case c.queue <- j:
		e.pending = done
		e.nextTry = now.Add(snapshot.RefreshInterval)
		return true
	default:
		c.stats.Backpressure++
		return false
	}
}

// Get serves usable stale data while one bounded refresh runs. Cold/expired
// callers wait on the same job; cancellation stops only their own wait.
func (c *Coordinator) Get(ctx context.Context, k snapshot.Key) (*snapshot.Snapshot, error) {
	if !validKey(k) {
		return nil, snapshot.ErrInvalid
	}
	waited := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.closed || c.ctx.Err() != nil {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		now := c.options.Now()
		e := c.entries[k]
		if e == nil {
			if len(c.entries) >= c.options.MaxEntries && !c.evict(nil) {
				c.stats.Backpressure++
				c.mu.Unlock()
				return nil, ErrBusy
			}
			e = &entry{}
			c.entries[k] = e
		}
		e.access = now
		if usable(e, now) {
			if e.value.NeedsRefresh(now) && !now.Before(e.nextTry) {
				c.enqueue(k, e, now)
			}
			if !waited {
				c.stats.MemoryHits++
				if e.value.NeedsRefresh(now) {
					c.stats.StaleHits++
				}
			}
			value := e.value
			c.mu.Unlock()
			return value, nil
		}
		if !waited {
			c.stats.MemoryMisses++
		}
		if e.pending == nil && now.Before(e.nextTry) {
			value, err := e.value, e.err
			if err == nil {
				err = ErrUnavailable
			}
			if e.expired {
				c.stats.Expired++
			}
			c.mu.Unlock()
			return value, err
		}
		joined := e.pending != nil
		if e.pending == nil && !c.enqueue(k, e, now) {
			value := e.value
			c.mu.Unlock()
			return value, ErrBusy
		}
		pending := e.pending
		waited = true
		if joined {
			c.stats.Coalesced++
		}
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.ctx.Done():
			return nil, ErrClosed
		case <-pending:
		}
	}
}

// Invalidate is called only after a committed mutation. A local reader must
// reverify with PostgreSQL; an older in-flight load cannot satisfy this fence.
func (c *Coordinator) Invalidate(k snapshot.Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[k]; e != nil {
		e.epoch++
		e.force = true
		e.nextTry = time.Time{}
		e.err = nil
	}
}

// Poll schedules active keys only. No goroutine is created per key or request.
func (c *Coordinator) Poll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.ctx.Err() != nil {
		return
	}
	now := c.options.Now()
	for k, e := range c.entries {
		if now.Sub(e.access) < snapshot.MaxAge && e.pending == nil && !now.Before(e.nextTry) &&
			(e.force || e.value == nil || e.value.NeedsRefresh(now)) {
			c.enqueue(k, e, now)
		}
	}
}
func (c *Coordinator) poller() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.options.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.Poll()
		}
	}
}
func (c *Coordinator) worker() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case j := <-c.queue:
			c.refresh(j)
		}
	}
}
func (c *Coordinator) storeRead(k snapshot.Key) (*snapshot.Snapshot, error) {
	ctx, cancel := context.WithTimeout(c.ctx, c.options.StoreTimeout)
	defer cancel()
	return c.store.Get(ctx, k)
}
func (c *Coordinator) refresh(j job) {
	var value *snapshot.Snapshot
	var err error
	authoritative := false
	if j.cold && c.store != nil {
		value, err = c.storeRead(j.key)
		if err != nil || value == nil || value.Key() != j.key || !value.Usable(c.options.Now()) {
			value = nil
		}
		c.mu.Lock()
		if value != nil {
			c.stats.RedisHits++
		} else if err != nil && !errors.Is(err, ErrMiss) {
			c.stats.StoreFailures++
		}
		c.mu.Unlock()
	}
	// Repeated Redis reads never count as re-verification. A stale Redis value
	// must attempt PostgreSQL, even if it still fits the last-known-good window.
	if value == nil || value.NeedsRefresh(c.options.Now()) {
		ctx, cancel := context.WithTimeout(c.ctx, c.options.SourceTimeout)
		proof := c.options.Now()
		c.mu.Lock()
		c.stats.SourceReads++
		c.mu.Unlock()
		var d evaluation.Definition
		d, err = c.source(ctx, j.key)
		if err == nil {
			err = ctx.Err()
		}
		cancel()
		if err == nil {
			var fresh *snapshot.Snapshot
			fresh, err = snapshot.New(d, proof)
			if err == nil && fresh.Key() != j.key {
				err = snapshot.ErrInvalid
			}
			if err == nil && !fresh.Usable(c.options.Now()) {
				err = snapshot.ErrExpired
			}
			if err == nil {
				if fresh.CanReplace(value) {
					value = fresh
					authoritative = true
				} else {
					err = ErrConflict
				}
			}
		}
		if err != nil && !errors.Is(err, ErrNotFound) {
			c.mu.Lock()
			c.stats.SourceFailures++
			c.mu.Unlock()
		}
	}
	// An invalidated job must not publish its old proof, even at the same revision.
	c.mu.Lock()
	valid := j.entry.epoch == j.epoch && !c.closed
	c.mu.Unlock()
	if valid && authoritative && c.store != nil {
		ctx, cancel := context.WithTimeout(c.ctx, c.options.StoreTimeout)
		_, storeErr := c.store.Put(ctx, value)
		cancel()
		if storeErr != nil {
			c.mu.Lock()
			c.stats.StoreFailures++
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := j.entry
	if c.entries[j.key] != e {
		return
	}
	if e.epoch != j.epoch {
		e.err = nil
		e.nextTry = time.Time{}
	} else {
		if errors.Is(err, ErrNotFound) {
			e.err = ErrNotFound
		} else if err != nil {
			e.err = ErrUnavailable
		} else {
			e.err = nil
		}
		if value != nil && value.Usable(c.options.Now()) && (value.CanReplace(e.value) || e.value == nil) {
			oldWeight := int64(0)
			if e.value != nil {
				oldWeight = e.value.Weight()
			}
			weight := value.Weight()
			for c.weight-oldWeight+weight > c.options.MaxWeight && c.evict(e) {
			}
			if c.weight-oldWeight+weight <= c.options.MaxWeight {
				c.weight = c.weight - oldWeight + weight
				e.value = value
				e.expired = false
				now := c.options.Now()
				e.expires = now.Add(value.Remaining(now))
			} else {
				c.stats.Backpressure++
				e.err = ErrBusy
				e.force = true
			}
		} else if value != nil && e.value != nil && value.Revision() < e.value.Revision() {
			c.stats.Regressions++
		}
		if authoritative && !errors.Is(e.err, ErrBusy) {
			e.force = false
		}
		if e.err == nil && !usable(e, c.options.Now()) {
			e.err = ErrUnavailable
		}
	}
	if e.pending != nil {
		close(e.pending)
		e.pending = nil
	}
}

func (c *Coordinator) Evaluate(ctx context.Context, k snapshot.Key, user string, attributes map[string]json.RawMessage, fallback evaluation.Value) (evaluation.Result, error) {
	if evaluation.ValidateContext(user, attributes) != nil || fallback.Validate(fallback.Type) != nil {
		return evaluation.Result{}, ErrInvalidFallback
	}
	s, err := c.Get(ctx, k)
	if s != nil && !evaluation.Equal(fallback, s.Safe()) {
		return evaluation.Result{}, ErrInvalidFallback
	}
	if errors.Is(err, ErrNotFound) {
		return evaluation.Result{Value: fallback, Reason: "flag_not_found"}, nil
	}
	if err != nil {
		reason := "cache_unavailable"
		if s != nil && !s.Usable(c.options.Now()) {
			reason = "cache_expired"
		}
		return evaluation.Result{Value: fallback, Reason: reason}, err
	}
	result, err := s.Evaluate(c.options.Now(), user, attributes)
	if errors.Is(err, snapshot.ErrExpired) {
		return evaluation.Result{Value: fallback, Reason: "cache_expired"}, err
	}
	return result, err
}
