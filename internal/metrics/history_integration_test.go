//go:build integration

package metrics

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"switchyard/internal/events"
)

func productRequest(exposure events.Event, id string, offset time.Duration, latency float64) events.Event {
	e := completion(exposure, id, offset)
	e.Kind = "request_outcome"
	bad := false
	e.IsError = &bad
	e.LatencyMS = &latency
	return e
}

func TestUnresolvedHistoryCannotSilentlyFreezePendingQuality(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	e := f.exposure(t, "display", f.user(t, "control", 0), 0)
	f.ingest(t, e)
	f.enqueueFacts(t)
	f.drain(t)
	before, err := f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE metric_user_state SET historical_contribution='{"quality":{"pending_outcomes":1}}',due_at=$1`, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err = ReconcileOne(ctx, f.pool, f.now); err == nil {
		t.Fatal("unresolved history was frozen")
	}
	after, err := f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed history merge changed counters", err)
	}
	var due int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM metric_user_state WHERE due_at IS NOT NULL`).Scan(&due); err != nil || due != 1 {
		t.Fatal("failed history merge lost durable work")
	}
}

func TestConversionCrossesRetainedBoundaryWithoutDoubleNumerator(t *testing.T) {
	for _, oldConverted := range []bool{false, true} {
		name := "first_conversion_retained"
		if oldConverted {
			name = "duplicate_conversion_retained"
		}
		t.Run(name, func(t *testing.T) {
			f := setupMeasurement(t)
			f.compareAggregates = false
			ctx := context.Background()
			exposure := f.exposure(t, "anchor", f.user(t, "control", 0), 2*time.Hour)
			f.ingest(t, exposure)
			if oldConverted {
				f.now = f.base.Add(2*time.Hour + time.Minute)
				f.ingest(t, completion(exposure, "old_completion", time.Minute))
			}
			// Exactly 24 hours late: the exposure is on the old side of retention,
			// while this eligible completion remains in the raw-retained interval.
			f.now = f.base.Add(26*time.Hour + time.Minute)
			f.ingest(t, completion(exposure, "retained_completion", time.Minute))
			cutoff := f.base.Add(26 * time.Hour)
			f.now = cutoff.Add(7 * 24 * time.Hour)
			expected := f.read(t)
			f.enqueueFacts(t)
			f.drain(t)
			var baseline []byte
			historySQL := strings.Replace(attributionSQL, "AND received_at<=$4", "AND received_at<$5", 1)
			if err := f.pool.QueryRow(ctx, historySQL+resultsSQL, f.projectID, f.env, f.runID, f.now, cutoff).Scan(&baseline); err != nil {
				t.Fatal(err)
			}
			tx, err := f.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if _, err = tx.Exec(ctx, `UPDATE metric_user_state SET historical_contribution=$2 WHERE user_id=$1`, exposure.UserID, baseline); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `INSERT INTO metric_archived_anchors(project_id,environment_id,run_id,user_id,event_id,variant_id,occurred_at,received_at) SELECT project_id,environment_id,run_id,user_id,event_id,variant_id,occurred_at,received_at FROM raw_events WHERE event_id='anchor'`); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `INSERT INTO metric_event_references SELECT project_id,environment_id,event_id,run_id,user_id,kind,status,variant_id,occurred_at,received_at FROM raw_events WHERE received_at<$1`, cutoff); err != nil {
				t.Fatal(err)
			}
			if _, err = tx.Exec(ctx, `DELETE FROM raw_events WHERE received_at<$1`, cutoff); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			f.enqueueFacts(t)
			f.drain(t)
			actual, err := f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("boundary contribution changed\nexpected=%+v\nactual=%+v", expected, actual)
			}
			wantDuplicates := int64(0)
			if oldConverted {
				wantDuplicates = 1
			}
			if variant(t, actual, "control").Total != (Counts{1, 1}) || actual.Quality.DuplicateAttributedCompletions != wantDuplicates {
				t.Fatal("conversion uniqueness changed")
			}
		})
	}
}

// Only this disposable schema deletes raw facts. Normal cleanup stays disabled
// until the full raw-retained folding/deduplication/expiry gates are implemented.
func TestHistoricalContributionSurvivesRawDeletionAndReturningUser(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	exposure := f.exposure(t, "old_exposure", f.user(t, "control", 0), 0)
	oldRequest := productRequest(exposure, "old_request", time.Minute, 80)
	f.ingest(t, exposure, completion(exposure, "old_conversion", time.Minute), oldRequest)
	f.now = f.base.Add(8*24*time.Hour + 2*time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	var baseline []byte
	if err := f.pool.QueryRow(ctx, `SELECT contribution FROM metric_user_state WHERE user_id=$1`, exposure.UserID).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	returning := f.exposure(t, "returning_exposure", exposure.UserID, 8*24*time.Hour)
	newRequest := productRequest(exposure, "new_request", 8*24*time.Hour+time.Minute, 600)
	f.ingest(t, returning, newRequest, completion(exposure, "outside_window", 8*24*time.Hour+time.Minute))
	// A new user's accepted outcome points to a purged non-exposure: preserve
	// invalid-reference classification, rather than making it pending again.
	wrong := completion(oldRequest, "wrong_kind_reference", 8*24*time.Hour+time.Minute)
	wrong.UserID = f.user(t, "control", 1)
	wrong.ExposureID = oldRequest.ID
	wrong.IsError = nil
	wrong.LatencyMS = nil
	f.ingest(t, wrong)
	expected := f.read(t)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE metric_user_state SET historical_contribution=$2 WHERE user_id=$1`, exposure.UserID, baseline); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO metric_archived_anchors(project_id,environment_id,run_id,user_id,event_id,variant_id,occurred_at,received_at) SELECT project_id,environment_id,run_id,user_id,event_id,variant_id,occurred_at,received_at FROM raw_events WHERE event_id='old_exposure'`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO metric_event_references SELECT project_id,environment_id,event_id,run_id,user_id,kind,status,variant_id,occurred_at,received_at FROM raw_events WHERE received_at<$1`, f.now.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM raw_events WHERE received_at<$1`, f.now.Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.enqueueFacts(t)
	f.drain(t)
	actual, err := f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("historical/returning-user results changed\nexpected=%+v\nactual=%+v", expected, actual)
	}
	f.enqueueFacts(t)
	f.drain(t)
	repeated, err := f.metrics.ReadAggregated(ctx, f.actor, f.projectID, f.runID)
	if err != nil || !reflect.DeepEqual(repeated, expected) {
		t.Fatal("historical contribution counted again", err)
	}
}
