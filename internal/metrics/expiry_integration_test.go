//go:build integration

package metrics

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"switchyard/internal/events"
)

func (f *measurementFixture) foldAll(t *testing.T) {
	t.Helper()
	for i := 0; i < 100; i++ {
		r, err := FoldOne(context.Background(), f.pool, f.now)
		if err != nil {
			t.Fatal(err)
		}
		f.drain(t)
		if r == (FoldOutcome{}) {
			return
		}
	}
	t.Fatal("folding did not drain bounded fixture")
}

func TestSummaryExpiryBoundariesAndReturningUserCannotResurrectCohort(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	f.base = time.Now().UTC().Truncate(24 * time.Hour)
	f.now = f.base.Add(2 * time.Hour)
	e := f.exposure(t, "original", f.user(t, "control", 0), 0)
	f.ingest(t, e, completion(e, "converted", time.Minute), productRequest(e, "old_request", time.Minute, 100))
	f.now = f.base.Add(26 * time.Hour)
	later := productRequest(e, "later_request", 25*time.Hour, 500)
	unresolved := completion(e, "unresolved", 25*time.Hour)
	unresolved.ExposureID = "missing"
	f.ingest(t, later, unresolved)
	f.processingReceipts(t)
	f.now = f.base.Add(8*24*time.Hour + 3*time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	f.foldAll(t)
	f.now = f.base.Add(90*24*time.Hour - time.Microsecond)
	if r, err := ExpireOne(ctx, f.pool, f.now, 90); err != nil || r != (ExpiredSummary{}) {
		t.Fatal("early calendar expiry", r, err)
	}
	f.now = f.base.Add(90 * 24 * time.Hour)
	// Floor changes must roll back with summary deletion.
	if _, err := f.pool.Exec(ctx, `CREATE FUNCTION reject_expiry_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'expiry fixture'; END; $$;
 CREATE TRIGGER expiry_fixture BEFORE DELETE ON metric_history_segments FOR EACH ROW EXECUTE FUNCTION reject_expiry_fixture()`); err != nil {
		t.Fatal(err)
	}
	if _, err := ExpireOne(ctx, f.pool, f.now, 90); err == nil {
		t.Fatal("failed expiry accepted")
	}
	var unchanged bool
	if err := f.pool.QueryRow(ctx, `SELECT reporting_since='-infinity'::timestamptz FROM metric_user_state`).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("failed expiry advanced floor", err)
	}
	if _, err := f.pool.Exec(ctx, `DROP TRIGGER expiry_fixture ON metric_history_segments`); err != nil {
		t.Fatal(err)
	}
	var g sync.WaitGroup
	var mu sync.Mutex
	users := 0
	for range 8 {
		g.Go(func() {
			r, err := ExpireOne(ctx, f.pool, f.now, 90)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			if r.User {
				users++
			}
			mu.Unlock()
		})
	}
	g.Wait()
	if users != 1 {
		t.Fatal("concurrent expiry did not serialize", users)
	}
	f.drain(t)
	result, err := f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	control := variant(t, result, "control")
	if control.Total != (Counts{}) || control.Requests.Count != 1 || result.Quality.PendingOutcomes != 1 {
		t.Fatal("wrong partial reporting expiry", control, result.Quality)
	}
	var anchors, references int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM metric_archived_anchors),(SELECT count(*) FROM metric_event_references)`).Scan(&anchors, &references); err != nil || anchors != 1 || references != 5 {
		t.Fatal("attribution identity lost", anchors, references, err)
	}
	returning := e
	returning.ID = "returning"
	returning.OccurredAt = f.now.Add(-time.Minute)
	fresh := productRequest(returning, "fresh_request", time.Second, 50)
	f.ingest(t, returning, fresh)
	f.processingReceipts(t)
	f.enqueueFacts(t)
	f.drain(t)
	result, err = f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	control = variant(t, result, "control")
	if control.Total != (Counts{}) || control.Requests.Count != 2 {
		t.Fatal("returning user recreated expired cohort", control)
	}
	f.now = f.base.Add(91 * 24 * time.Hour)
	if r, err := ExpireOne(ctx, f.pool, f.now, 90); err != nil || !r.User || r.Pending != 1 || r.Segments != 1 {
		t.Fatal("second receipt day expiry", r, err)
	}
	f.drain(t)
	result, err = f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if variant(t, result, "control").Requests.Count != 1 || result.Quality.PendingOutcomes != 0 {
		t.Fatal("pending/request summary did not expire", result)
	}
	// Increasing the retention window does not reconstruct already-expired data.
	if r, err := ExpireOne(ctx, f.pool, f.now, 365); err != nil || r != (ExpiredSummary{}) {
		t.Fatal("expiry floor regressed", r, err)
	}
}

func TestSummaryExpiryBoundsPendingDeletionAndKeepsCountersStable(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	f.base = time.Now().UTC().Truncate(24 * time.Hour)
	f.now = f.base.Add(2 * time.Hour)
	e := f.exposure(t, "missing", f.user(t, "control", 0), 0)
	facts := make([]events.Event, 105)
	for i := range facts {
		facts[i] = completion(e, fmt.Sprintf("pending_%03d", i), time.Minute)
	}
	f.ingest(t, facts[:100]...)
	f.ingest(t, facts[100:]...)
	f.processingReceipts(t)
	f.now = f.base.Add(8 * 24 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	f.foldAll(t)
	f.now = f.base.Add(90 * 24 * time.Hour)
	first, err := ExpireOne(ctx, f.pool, f.now, 90)
	if err != nil || first.Pending != 100 || first.Segments != 1 {
		t.Fatal("unbounded first expiry", first, err)
	}
	f.drain(t)
	result, err := f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
	if err != nil || result.Quality.PendingOutcomes != 0 {
		t.Fatal("floor still reports old unresolved work", result, err)
	}
	second, err := ExpireOne(ctx, f.pool, f.now, 90)
	if err != nil || second.Pending != 5 || second.Segments != 0 {
		t.Fatal("remaining pending page lost", second, err)
	}
	f.drain(t)
	if r, err := ExpireOne(ctx, f.pool, f.now, 90); err != nil || r != (ExpiredSummary{}) {
		t.Fatal("expiry not idempotent", r, err)
	}
}

func TestSummaryExpiryBoundsHistoricalDayDeletion(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	e := f.exposure(t, "anchor", f.user(t, "control", 0), 0)
	f.ingest(t, e)
	f.processingReceipts(t)
	f.now = f.now.Add(8 * 24 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	f.foldAll(t)
	// A long-disabled maintenance loop can leave more than 100 old daily rows.
	if _, err := f.pool.Exec(ctx, `INSERT INTO metric_history_segments(project_id,environment_id,run_id,user_id,receipt_day,contribution)
 SELECT $1,$2,$3,$4,($5 AT TIME ZONE 'UTC')::date-i,'{}'::jsonb FROM generate_series(1,110) i`, f.projectID, f.env, f.runID, e.UserID, f.base); err != nil {
		t.Fatal(err)
	}
	f.now = f.base.Add(100 * 24 * time.Hour)
	first, err := ExpireOne(ctx, f.pool, f.now, 90)
	if err != nil || first.Segments != 100 {
		t.Fatal("historical deletion exceeded page", first, err)
	}
	f.drain(t)
	second, err := ExpireOne(ctx, f.pool, f.now, 90)
	if err != nil || second.Segments != 11 {
		t.Fatal("historical tail lost after floor advance", second, err)
	}
	f.drain(t)
	if r, err := ExpireOne(ctx, f.pool, f.now, 90); err != nil || r != (ExpiredSummary{}) {
		t.Fatal("historical expiry did not finish", r, err)
	}
}
