//go:build integration

package processing

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"switchyard/internal/outbox"
	"switchyard/internal/platform/messaging"
)

func TestCompletedWorkExpiryPreservesPendingSourcesAndReplaySafety(t *testing.T) {
	p, s := setup(t)
	ctx := context.Background()
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	now := stamp.Add(ReceiptRetention)
	names := []string{"complete", "raw_retained", "unpublished", "unresolved_failure", "recent_receipt", "recent_publication", "leased", "unprocessed"}
	envelopes := map[string]messaging.Envelope{}
	for _, name := range names {
		e := fact(t, p, name, name)
		envelopes[name] = e
		if name != "unprocessed" {
			if _, err := s.Apply(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := p.Exec(ctx, `WITH changed AS (UPDATE outbox SET created_at=$1,published_at=$1 RETURNING id) UPDATE processed_work SET processed_at=$1`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `DELETE FROM raw_events WHERE event_id<>'raw_retained';
 UPDATE outbox SET published_at=NULL WHERE object_id='unpublished';
 UPDATE outbox SET claim_token='fixture',lease_until=now()+interval '1 hour' WHERE object_id='leased'`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE processed_work SET processed_at=$1 WHERE message_id=$2`, stamp.Add(time.Microsecond), envelopes["recent_receipt"].MessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE outbox SET published_at=$1 WHERE object_id='recent_publication'`, stamp.Add(time.Microsecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeadLetter(ctx, "STREAM", 1, envelopes["unresolved_failure"].MessageID, []byte("fixture"), "source_missing"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeadLetter(ctx, "STREAM", 2, "resolved", []byte("fixture"), "source_missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `UPDATE work_dead_letters SET resolved_at=$1 WHERE stream_sequence=2`, stamp); err != nil {
		t.Fatal(err)
	}
	if result, err := PruneCompleted(ctx, p, now.Add(-time.Microsecond), 100); err != nil || result != (PrunedWork{}) {
		t.Fatal("early expiry", result, err)
	}
	// Fail after receipt deletion: the entire publication/receipt pair survives.
	if _, err := p.Exec(ctx, `CREATE FUNCTION reject_prune_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'prune fixture'; END; $$;
 CREATE TRIGGER prune_fixture BEFORE DELETE ON outbox FOR EACH ROW EXECUTE FUNCTION reject_prune_fixture()`); err != nil {
		t.Fatal(err)
	}
	if _, err := PruneCompleted(ctx, p, now, 100); err == nil {
		t.Fatal("prune failure accepted")
	}
	var receipts int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM processed_work WHERE message_id=$1`, envelopes["complete"].MessageID).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal("failed delete lost receipt", err)
	}
	if _, err := p.Exec(ctx, `DROP TRIGGER prune_fixture ON outbox`); err != nil {
		t.Fatal(err)
	}
	result, err := PruneCompleted(ctx, p, now, 100)
	if err != nil || result != (PrunedWork{1, 1, 1}) {
		t.Fatal("unsafe expiry", result, err)
	}
	if _, err := s.Apply(ctx, envelopes["complete"]); !errors.Is(err, ErrSourceMissing) {
		t.Fatal("expired broker replay applied", err)
	}
	var pending, used int
	if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM work_dead_letters),used FROM work_capacity WHERE name='dead_letters'`).Scan(&pending, &used); err != nil || pending != 1 || used != 1 {
		t.Fatal("dead letter capacity mismatch", pending, used, err)
	}
	if duplicate, err := s.Apply(ctx, envelopes["unresolved_failure"]); err != nil || !duplicate {
		t.Fatal("protected identity lost", err)
	}
	for _, limit := range []int{0, 101} {
		if _, err := PruneCompleted(ctx, p, now, limit); err == nil {
			t.Fatal("unbounded cleanup permitted")
		}
	}
}

func TestCompletedWorkExpiryPagesAndConcurrentPruners(t *testing.T) {
	p, s := setup(t)
	ctx := context.Background()
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	for i := range 105 {
		e := fact(t, p, fmt.Sprintf("expired_%03d", i), fmt.Sprintf("user_%03d", i))
		if _, err := s.Apply(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Exec(ctx, `WITH removed AS (DELETE FROM raw_events RETURNING event_id), changed AS (UPDATE outbox SET created_at=$1,published_at=$1 RETURNING id) UPDATE processed_work SET processed_at=$1`, stamp); err != nil {
		t.Fatal(err)
	}
	now := stamp.Add(ReceiptRetention)
	var g sync.WaitGroup
	var mu sync.Mutex
	var total PrunedWork
	for range 3 {
		g.Go(func() {
			r, err := PruneCompleted(ctx, p, now, 100)
			if err != nil {
				t.Error(err)
				return
			}
			if r.Publications > 100 {
				t.Error("unbounded page")
			}
			mu.Lock()
			total.Publications += r.Publications
			total.Receipts += r.Receipts
			mu.Unlock()
		})
	}
	g.Wait()
	if total != (PrunedWork{105, 105, 0}) {
		t.Fatal("concurrent cleanup duplicated/skipped pairs", total)
	}
	if r, err := PruneCompleted(ctx, p, now, 100); err != nil || r != (PrunedWork{}) {
		t.Fatal("repeated cleanup changed state", r, err)
	}
	var states, receipts int
	if err := p.QueryRow(ctx, `SELECT (SELECT count(*) FROM metric_user_state),(SELECT count(*) FROM processed_work)`).Scan(&states, &receipts); err != nil || states != 105 || receipts != 0 {
		t.Fatal("cleanup changed aggregate work", states, receipts, err)
	}
}

func TestConfigurationExpiryKeepsHistoryAndSkipsInFlightSourceLock(t *testing.T) {
	p, s := setup(t)
	ctx := context.Background()
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	var id int64
	if err := p.QueryRow(ctx, `INSERT INTO outbox(kind,project_id,environment_id,object_id,revision) VALUES('configuration','project','dev','flag',1) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	e := messaging.Envelope{Version: 1, MessageID: fmt.Sprintf("switchyard-outbox-v1-%d", id), Reference: outbox.Reference{Kind: "configuration", ProjectID: "project", EnvironmentID: "dev", ObjectID: "flag", Revision: 1}}
	if _, err := s.Apply(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Exec(ctx, `WITH changed AS (UPDATE outbox SET created_at=$1,published_at=$1 RETURNING id) UPDATE processed_work SET processed_at=$1`, stamp); err != nil {
		t.Fatal(err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT id FROM outbox WHERE id=$1 FOR KEY SHARE`, id); err != nil {
		t.Fatal(err)
	}
	now := stamp.Add(ReceiptRetention)
	if r, err := PruneCompleted(ctx, p, now, 100); err != nil || r != (PrunedWork{}) {
		t.Fatal("in-flight publication removed", r, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r, err := PruneCompleted(ctx, p, now, 100); err != nil || r != (PrunedWork{1, 1, 0}) {
		t.Fatal("configuration history cleanup", r, err)
	}
	var revisions int
	if err = p.QueryRow(ctx, `SELECT count(*) FROM flag_revisions`).Scan(&revisions); err != nil || revisions != 1 {
		t.Fatal("immutable configuration history deleted", err)
	}
	if _, err = s.Apply(ctx, e); !errors.Is(err, ErrSourceMissing) {
		t.Fatal("expired config notification recreated", err)
	}
}
