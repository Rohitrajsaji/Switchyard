//go:build integration

package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"switchyard/pkg/evaluation"
	"switchyard/pkg/snapshot"
)

func TestRedisMonotonicSnapshotsAndFreshness(t *testing.T) {
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("Set TEST_REDIS_URL for real Redis snapshot checks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// One test-owned key; never flush a database or inspect other applications.
	prefix := fmt.Sprintf("switchyard:test:%d:", time.Now().UnixNano())
	now := time.Now().UTC()
	store, err := NewRedis(url, prefix, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	k := snapshot.Key{ProjectID: "project", EnvironmentID: "development", FlagKey: "listing"}
	defer func() {
		if err := store.client.Del(context.Background(), store.key(k)).Err(); err != nil {
			t.Error(err)
		}
	}()
	value := evaluation.Value{Type: "boolean", Data: json.RawMessage("false")}
	d := evaluation.Definition{ProjectID: k.ProjectID, EnvironmentID: k.EnvironmentID, FlagID: "flag", Key: k.FlagKey,
		Type: "boolean", Revision: 9007199254740992, Default: value, Safe: value}
	makeSnapshot := func(d evaluation.Definition, verified time.Time) *snapshot.Snapshot {
		s, err := snapshot.New(d, verified)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	old := makeSnapshot(d, now)
	if _, err := store.Get(ctx, k); !errors.Is(err, ErrMiss) {
		t.Fatalf("missing=%v", err)
	}
	if applied, err := store.Put(ctx, old); err != nil || !applied {
		t.Fatalf("first=%v %v", applied, err)
	}
	d.Revision++ // Above 2^53: Lua numeric comparison would silently lose this increment.
	d.Killed = true
	latest := makeSnapshot(d, now)
	if applied, err := store.Put(ctx, latest); err != nil || !applied {
		t.Fatalf("disable=%v %v", applied, err)
	}
	var group sync.WaitGroup
	for i := 0; i < 12; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if applied, err := store.Put(ctx, old); err != nil || applied {
				t.Errorf("delayed writer=%v %v", applied, err)
			}
		}()
	}
	group.Wait()
	read, err := store.Get(ctx, k)
	if err != nil || read.Revision() != latest.Revision() {
		t.Fatalf("read=%v %v", read, err)
	}
	decision, err := read.Evaluate(now, "user", nil)
	if err != nil || decision.Reason != "kill_switch" {
		t.Fatalf("disable lost=%+v %v", decision, err)
	}
	d.Killed = false
	if _, err := store.Put(ctx, makeSnapshot(d, now)); !errors.Is(err, ErrConflict) {
		t.Fatalf("same revision mutation=%v", err)
	}
	d.Killed = true
	otherFlag := d
	otherFlag.FlagID = "another_flag"
	otherFlag.Revision++
	if _, err := store.Put(ctx, makeSnapshot(otherFlag, now)); !errors.Is(err, ErrConflict) {
		t.Fatalf("unrelated flag identity replaced scope=%v", err)
	}
	d.Revision = math.MaxInt64
	if applied, err := store.Put(ctx, makeSnapshot(d, now)); err != nil || !applied {
		t.Fatalf("max revision=%v %v", applied, err)
	}
	// Inject a local clock; no real-time sleeps. Reads retain the original time.
	now = now.Add(20 * time.Second)
	read, err = store.Get(ctx, k)
	if err != nil || read.Remaining(now) != 10*time.Second {
		t.Fatalf("relay extended freshness=%v %v", read, err)
	}
	if applied, err := store.Put(ctx, read); err != nil || applied {
		t.Fatalf("same proof renewed TTL=%v %v", applied, err)
	}
	now = now.Add(10 * time.Second)
	if _, err := store.Get(ctx, k); !errors.Is(err, snapshot.ErrExpired) {
		t.Fatalf("expired cache served=%v", err)
	}
	if _, err := store.Put(ctx, read); !errors.Is(err, snapshot.ErrExpired) {
		t.Fatalf("expired writer=%v", err)
	}
	// Redis deletion is recoverable from a new authoritative proof, without any
	// persistence requirement on Redis itself.
	if err := store.client.Del(ctx, store.key(k)).Err(); err != nil {
		t.Fatal(err)
	}
	if applied, err := store.Put(ctx, makeSnapshot(d, now)); err != nil || !applied {
		t.Fatalf("repair=%v %v", applied, err)
	}
	wrong := k
	wrong.EnvironmentID = "production"
	if _, err := store.Get(ctx, wrong); !errors.Is(err, ErrMiss) {
		t.Fatalf("scope=%v", err)
	}
	// A payload copied into the wrong Redis key is rejected by envelope binding.
	body, _ := latest.MarshalJSON()
	if err := store.client.HSet(ctx, store.key(wrong), "payload", body).Err(); err != nil {
		t.Fatal(err)
	}
	defer store.client.Del(context.Background(), store.key(wrong))
	if _, err := store.Get(ctx, wrong); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("scope payload escaped=%v", err)
	}
	if err := store.client.HSet(ctx, store.key(k), "payload", "invalid").Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, k); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("corrupt cache served=%v", err)
	}
	// Leave a valid repair for the final TTL check.
	if err := store.client.Del(ctx, store.key(k)).Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, makeSnapshot(d, now.Add(-20*time.Second))); err != nil {
		t.Fatal(err)
	}
	ttl, err := store.client.PTTL(ctx, store.key(k)).Result()
	if err != nil || ttl <= 0 || ttl > 10*time.Second {
		t.Fatalf("remaining proof TTL=%v %v", ttl, err)
	}
	if _, err := store.client.HGet(ctx, store.key(k), "unexpected").Result(); !errors.Is(err, redis.Nil) {
		t.Fatalf("unexpected=%v", err)
	}
}
