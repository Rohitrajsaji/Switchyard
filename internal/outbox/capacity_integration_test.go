//go:build integration

package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"switchyard/internal/outbox"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentAdmissionCannotOverfillAndRollbackReleasesCapacity(t *testing.T) {
	p, s := setup(t)
	ctx := context.Background()
	exec(t, p, `UPDATE work_capacity SET maximum=3 WHERE name='publication'`)
	var admitted, rejected atomic.Int32
	var g sync.WaitGroup
	for i := range 30 {
		g.Go(func() {
			tx, err := p.Begin(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			defer tx.Rollback(ctx)
			err = outbox.Record(ctx, tx, outbox.Reference{Kind: "event", ProjectID: "p", EnvironmentID: "e", ObjectID: fmt.Sprint(i), Revision: 1})
			if errors.Is(err, outbox.ErrCapacity) {
				rejected.Add(1)
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			if err = tx.Commit(ctx); err != nil {
				t.Error(err)
				return
			}
			admitted.Add(1)
		})
	}
	g.Wait()
	if admitted.Load() != 3 || rejected.Load() != 27 {
		t.Fatalf("admission=%d rejected=%d", admitted.Load(), rejected.Load())
	}
	// Duplicate intent succeeds at capacity without consuming a slot.
	var object string
	if err := p.QueryRow(ctx, `SELECT object_id FROM outbox LIMIT 1`).Scan(&object); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = outbox.Record(ctx, tx, outbox.Reference{Kind: "event", ProjectID: "p", EnvironmentID: "e", ObjectID: object, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := s.Claim(ctx, 1, time.Second)
	if err != nil || len(items) != 1 {
		t.Fatal(err)
	}
	if err = s.Published(ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	record(t, p, "rolled-back", false)
	var used, actual int
	if err = p.QueryRow(ctx, `SELECT used,(SELECT count(*) FROM outbox WHERE published_at IS NULL) FROM work_capacity WHERE name='publication'`).Scan(&used, &actual); err != nil || used != 2 || actual != 2 {
		t.Fatalf("reservation leaked %d/%d %v", used, actual, err)
	}
	record(t, p, "after-release", true)
	if _, err = p.Exec(ctx, `TRUNCATE outbox`); err == nil {
		t.Fatal("truncate could desynchronize admission")
	}
}
