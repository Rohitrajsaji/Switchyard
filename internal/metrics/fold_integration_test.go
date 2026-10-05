//go:build integration

package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"switchyard/internal/events"
	"switchyard/pkg/evaluation"
)

func (f *measurementFixture) processingReceipts(t *testing.T) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), `INSERT INTO processed_work(message_id,reference)
 SELECT 'switchyard-outbox-v1-'||id,jsonb_build_object('kind',kind,'project_id',project_id,'environment_id',environment_id,'object_id',object_id,'revision',revision)
 FROM outbox WHERE kind='event' ON CONFLICT(message_id) DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
}
func (f *measurementFixture) createFoldOracle(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `CREATE TABLE fold_oracle_raw AS TABLE raw_events`); err != nil {
		t.Fatal(err)
	}
}
func (f *measurementFixture) refreshFoldOracle(t *testing.T) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO fold_oracle_raw SELECT r.* FROM raw_events r WHERE NOT EXISTS(
 SELECT 1 FROM fold_oracle_raw o WHERE o.project_id=r.project_id AND o.environment_id=r.environment_id AND o.event_id=r.event_id)`); err != nil {
		t.Fatal(err)
	}
}
func (f *measurementFixture) foldOracle(t *testing.T) Results {
	t.Helper()
	ctx := context.Background()
	var body, definitionBody []byte
	var control string
	if err := f.pool.QueryRow(ctx, `SELECT definition,control_variant_id FROM experiment_runs WHERE id=$1`, f.runID).Scan(&definitionBody, &control); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, strings.ReplaceAll(attributionSQL, "raw_events", "fold_oracle_raw")+resultsSQL, f.projectID, f.env, f.runID, f.now).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var d evaluation.Definition
	var raw derived
	if json.Unmarshal(definitionBody, &d) != nil || json.Unmarshal(body, &raw) != nil {
		t.Fatal("invalid oracle fixture")
	}
	result := assemble(d, control, f.now, raw)
	addStatistics(&result)
	return result
}
func (f *measurementFixture) foldParity(t *testing.T) {
	t.Helper()
	f.drain(t)
	got, err := f.metrics.ReadAggregated(context.Background(), f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if equal, err := f.metrics.Compare(context.Background(), f.actor, f.projectID, f.runID); err != nil || !equal {
		t.Fatal("retained parity gate rejected correct folded state", equal, err)
	}
	want := f.foldOracle(t)
	if !reflect.DeepEqual(got, want) {
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(got)
		t.Fatalf("fold parity\nraw=%s\nfolded=%s", a, b)
	}
}

func TestBoundedFoldingPreservesIndependentOracleAndRollsBackAllState(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	e := f.exposure(t, "anchor", f.user(t, "control", 0), 0)
	facts := []events.Event{e, completion(e, "conversion_a", time.Minute), completion(e, "conversion_b", 2*time.Minute)}
	for i := range 105 {
		facts = append(facts, productRequest(e, fmt.Sprintf("request_%03d", i), time.Minute, float64(50+i)))
	}
	f.ingest(t, facts[:100]...)
	f.ingest(t, facts[100:]...)
	f.processingReceipts(t)
	f.createFoldOracle(t)
	f.now = f.now.Add(8 * 24 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	f.foldParity(t)
	// A failure late in folding must not leave history or identities behind.
	if _, err := f.pool.Exec(ctx, `CREATE FUNCTION reject_fold_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'fold fixture deletion failure'; END; $$; CREATE TRIGGER fold_fixture BEFORE DELETE ON raw_events FOR EACH ROW EXECUTE FUNCTION reject_fold_fixture()`); err != nil {
		t.Fatal(err)
	}
	if _, err := FoldOne(ctx, f.pool, f.now); err == nil {
		t.Fatal("failed folding accepted")
	}
	var raw, identities, anchors, references, segments int
	if err := f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM raw_events),(SELECT count(*) FROM retained_event_identities),(SELECT count(*) FROM metric_archived_anchors),(SELECT count(*) FROM metric_event_references),(SELECT count(*) FROM metric_history_segments)`).Scan(&raw, &identities, &anchors, &references, &segments); err != nil || raw != 108 || identities+anchors+references+segments != 0 {
		t.Fatal("failed fold retained partial state")
	}
	if _, err := f.pool.Exec(ctx, `DROP TRIGGER fold_fixture ON raw_events`); err != nil {
		t.Fatal(err)
	}
	first, err := FoldOne(ctx, f.pool, f.now)
	if err != nil || first.Raw != 100 {
		t.Fatal("unbounded/failed first fold", first, err)
	}
	f.foldParity(t)
	second, err := FoldOne(ctx, f.pool, f.now)
	if err != nil || second.Raw != 8 {
		t.Fatal("second page lost", second, err)
	}
	f.foldParity(t)
	empty, err := FoldOne(ctx, f.pool, f.now)
	if err != nil || empty != (FoldOutcome{}) {
		t.Fatal("fold counted the same source twice", empty, err)
	}
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM raw_events),(SELECT count(*) FROM retained_event_identities),(SELECT count(*) FROM metric_archived_anchors),(SELECT count(*) FROM metric_event_references),(SELECT count(*) FROM metric_history_segments)`).Scan(&raw, &identities, &anchors, &references, &segments); err != nil || raw != 0 || identities != 108 || anchors != 1 || references != 108 || segments != 1 {
		t.Fatalf("fold counts=%d %d %d %d %d %v", raw, identities, anchors, references, segments, err)
	}
	served, err := f.metrics.ReadAsync(ctx, f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	control := variant(t, served, "control")
	if served.Processing == nil || served.Processing.PendingEvents != 0 || served.Processing.DueUsers != 0 || control.Finalized.Exposed != 1 || control.Finalized.Converted != 1 || control.Requests.Count != 105 {
		t.Fatal("Serving read lost retained history or reported false backlog", served)
	}
}

func TestPendingOutcomeSurvivesFoldAndNotificationThenBecomesHistorical(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	e := f.exposure(t, "late_reference", f.user(t, "control", 0), 0)
	outcome := completion(e, "pending_completion", time.Minute)
	f.ingest(t, outcome)
	f.processingReceipts(t)
	f.createFoldOracle(t)
	f.now = f.now.Add(8 * 24 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	folded, err := FoldOne(ctx, f.pool, f.now)
	if err != nil || folded.Raw != 1 || folded.PreservedPending != 1 {
		t.Fatal("pending outcome discarded", folded, err)
	}
	f.foldParity(t)
	// A fresh exposure cannot retroactively make an eight-day-old completion
	// valid. Its arrival must still notify the compact outcome's user.
	e.OccurredAt = f.now.Add(-time.Minute)
	f.ingest(t, e)
	f.processingReceipts(t)
	f.refreshFoldOracle(t)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = EnqueueReferencingUsers(ctx, tx, f.projectID, f.env, e.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.foldParity(t)
	resolved, err := FoldOne(ctx, f.pool, f.now)
	if err != nil || resolved.ResolvedPending != 1 || resolved.Raw != 0 {
		t.Fatal("resolved compact outcome not folded", resolved, err)
	}
	f.foldParity(t)
	var pending int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM metric_pending_outcomes`).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("resolved compact work stranded")
	}
}

func TestFoldingWaitsForReceiptAndConcurrentFoldersShareUserLock(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	e := f.exposure(t, "delivered", f.user(t, "control", 0), 0)
	f.ingest(t, e)
	f.createFoldOracle(t)
	f.now = f.now.Add(8 * 24 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	if r, err := FoldOne(ctx, f.pool, f.now); err != nil || r != (FoldOutcome{}) {
		t.Fatal("undelivered fact was purged", r, err)
	}
	f.processingReceipts(t)
	var g sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for range 8 {
		g.Go(func() {
			r, err := FoldOne(ctx, f.pool, f.now)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			total += r.Raw
			mu.Unlock()
		})
	}
	g.Wait()
	if total != 1 {
		t.Fatal("concurrent folders duplicated fact", total)
	}
	f.foldParity(t)
}

func TestFoldingUsesLateEarlierAnchorAcrossUTCReceiptDays(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	f.base = time.Now().UTC().Truncate(24 * time.Hour)
	f.now = f.base.Add(2 * time.Hour)
	user := f.user(t, "control", 0)
	later := f.exposure(t, "later_anchor", user, 20*time.Minute)
	f.ingest(t, later, completion(later, "corrected_conversion", 20*time.Minute))
	f.now = f.base.Add(24 * time.Hour)
	earlier := f.exposure(t, "earlier_anchor", user, 0)
	f.ingest(t, earlier)
	f.processingReceipts(t)
	f.createFoldOracle(t)
	f.now = f.now.Add(8 * 24 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	first, err := FoldOne(ctx, f.pool, f.now)
	if err != nil || first.Raw != 2 {
		t.Fatal("UTC day folding bound changed", first, err)
	}
	f.foldParity(t)
	second, err := FoldOne(ctx, f.pool, f.now)
	if err != nil || second.Raw != 1 {
		t.Fatal("second receipt day lost", second, err)
	}
	f.foldParity(t)
	var anchor string
	if err = f.pool.QueryRow(ctx, `SELECT event_id FROM metric_archived_anchors`).Scan(&anchor); err != nil || anchor != earlier.ID {
		t.Fatal("later receipt restored the wrong anchor")
	}
	actual, err := f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
	if err != nil || variant(t, actual, "control").Total != (Counts{1, 0}) || actual.Quality.OutsideWindowCompletions != 1 {
		t.Fatal("late earlier exposure correction disappeared", err)
	}
}

func TestConfiguredRawRetentionControlsFoldBoundary(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	e := f.exposure(t, "configured", f.user(t, "control", 0), 0)
	f.ingest(t, e)
	f.processingReceipts(t)
	f.now = f.now.Add(3 * 24 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	if r, err := FoldOne(ctx, f.pool, f.now); err != nil || r != (FoldOutcome{}) {
		t.Fatal("default raw horizon shortened", r, err)
	}
	if r, err := FoldWithRetention(ctx, f.pool, f.now, 2); err != nil || r.Raw != 1 {
		t.Fatal("configured raw horizon ignored", r, err)
	}
	f.drain(t)
	if equal, err := f.metrics.Compare(ctx, f.actor, f.projectID, f.runID); err != nil || !equal {
		t.Fatal("configured fold parity", equal, err)
	}
	for _, days := range []int{1, 8} {
		if _, err := FoldWithRetention(ctx, f.pool, f.now, days); err == nil {
			t.Fatal("unsafe raw horizon")
		}
	}
}
