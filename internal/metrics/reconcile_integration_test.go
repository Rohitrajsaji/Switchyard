//go:build integration

package metrics

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"
)

// This schedules oracle fixture facts, separately from the processing receipt
// tests. Every existing measurement scenario can then compare materialization.
func (f *measurementFixture) enqueueFacts(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO metric_user_state(project_id,environment_id,run_id,user_id,due_at)
        SELECT project_id,environment_id,run_id,user_id,clock_timestamp() FROM
        (SELECT DISTINCT project_id,environment_id,run_id,user_id FROM raw_events) users
        ON CONFLICT(project_id,environment_id,run_id,user_id) DO UPDATE SET due_at=excluded.due_at`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
func (f *measurementFixture) drain(t *testing.T) {
	t.Helper()
	for n := 0; n < 1000; n++ {
		worked, err := ReconcileOne(context.Background(), f.pool, f.now)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("metric reconciliation did not drain its bounded fixture")
}
func (f *measurementFixture) parity(t *testing.T, want Results) {
	t.Helper()
	got, err := f.metrics.ReadAggregated(context.Background(), f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(got)
		t.Fatalf("aggregate parity mismatch\nraw=%s\nagg=%s", a, b)
	}
	if equal, err := f.metrics.Compare(context.Background(), f.actor, f.projectID, f.runID); err != nil || !equal {
		t.Fatalf("repeatable-read parity gate=%v %v", equal, err)
	}
}
func TestReconciliationSchedulesFutureAndExactFinalizationBoundary(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	future := f.exposure(t, "future", f.user(t, "control", 0), 2*time.Hour+time.Minute)
	f.ingest(t, future)
	// The operations gate must detect missing materialization rather than pass
	// merely because both representations can be read.
	f.now = future.OccurredAt
	if equal, err := f.metrics.Compare(context.Background(), f.actor, f.projectID, f.runID); err != nil || equal {
		t.Fatalf("missing materialization accepted=%v %v", equal, err)
	}
	f.now = f.base.Add(2 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	f.parity(t, f.read(t))
	f.now = future.OccurredAt
	f.drain(t)
	f.parity(t, f.read(t))
	if variant(t, f.read(t), "control").Total.Exposed != 1 {
		t.Fatal("future fact stayed hidden")
	}
	f.now = future.OccurredAt.Add(24*time.Hour + 30*time.Minute)
	f.drain(t)
	f.parity(t, f.read(t))
	if variant(t, f.read(t), "control").Finalized.Exposed != 0 {
		t.Fatal("inclusive boundary finalized early")
	}
	f.now = f.now.Add(time.Microsecond)
	f.drain(t)
	f.parity(t, f.read(t))
	if variant(t, f.read(t), "control").Finalized.Exposed != 1 {
		t.Fatal("finalization deadline not scheduled")
	}
}
func TestConcurrentReconciliationAndRepeatedIntentDoNotDoubleCount(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	e := f.exposure(t, "display", f.user(t, "control", 0), 0)
	f.ingest(t, e, completion(e, "conversion", time.Minute))
	f.enqueueFacts(t)
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			_, err := ReconcileOne(context.Background(), f.pool, f.now)
			if err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	f.parity(t, f.read(t))
	f.enqueueFacts(t)
	f.drain(t)
	f.parity(t, f.read(t))
	if variant(t, f.read(t), "control").Total != (Counts{1, 1}) {
		t.Fatal("duplicate contribution changed counts")
	}
	// Counter update, contribution and queue completion must roll back together.
	if _, err := f.pool.Exec(context.Background(), `CREATE FUNCTION reject_contribution_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'contribution fixture failure'; END; $$; CREATE TRIGGER contribution_fixture BEFORE UPDATE ON metric_user_state FOR EACH ROW EXECUTE FUNCTION reject_contribution_fixture()`); err != nil {
		t.Fatal(err)
	}
	// A new user's first contribution would increment counters before this trigger.
	other := f.exposure(t, "other", f.user(t, "control", 1), 0)
	f.ingest(t, other)
	tx, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = EnqueueUser(context.Background(), tx, f.projectID, f.env, f.runID, other.UserID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = ReconcileOne(context.Background(), f.pool, f.now); err == nil {
		t.Fatal("failed contribution commit accepted")
	}
	result, err := f.metrics.ReadAggregated(context.Background(), f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if variant(t, result, "control").Total != (Counts{1, 1}) {
		t.Fatal("failed transaction partially incremented counts")
	}
}

func TestFutureReceiptClockCannotStrandDurableWork(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	e := f.exposure(t, "receipt_clock", f.user(t, "control", 0), 0)
	received := f.now
	f.ingest(t, e)
	f.enqueueFacts(t)
	f.now = f.base.Add(time.Hour)
	f.drain(t)
	f.parity(t, f.read(t))
	if variant(t, f.read(t), "control").Total.Exposed != 0 {
		t.Fatal("future receipt was counted before its as-of time")
	}
	f.now = received
	f.drain(t)
	f.parity(t, f.read(t))
	if variant(t, f.read(t), "control").Total.Exposed != 1 {
		t.Fatal("future receipt permanently stranded accepted work")
	}
}

func TestFutureReferencedReceiptReschedulesAnotherUser(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	source := f.exposure(t, "referenced_clock", f.user(t, "control", 0), 0)
	outcome := completion(source, "wrong_user_outcome", time.Minute)
	outcome.UserID = f.user(t, "control", 1)
	f.now = f.base.Add(time.Hour)
	f.ingest(t, outcome)
	f.enqueueFacts(t)
	f.drain(t)
	f.parity(t, f.read(t))
	f.now = f.base.Add(2 * time.Hour)
	f.ingest(t, source)
	f.enqueueFacts(t)
	f.now = f.base.Add(time.Hour)
	f.drain(t)
	f.parity(t, f.read(t))
	f.now = f.base.Add(2 * time.Hour)
	f.drain(t)
	f.parity(t, f.read(t))
	if r := f.read(t); r.Quality.PendingOutcomes != 0 || r.Quality.InvalidReferenceOutcomes != 1 {
		t.Fatal("future referenced receipt did not repair another user's quality state")
	}
}
