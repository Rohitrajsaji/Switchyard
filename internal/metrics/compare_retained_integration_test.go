//go:build integration

package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"switchyard/internal/events"
)

func TestRetainedParityDetectsHistoricalCacheAndCounterCorruption(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	e := f.exposure(t, "archived", f.user(t, "control", 0), 0)
	f.ingest(t, e, completion(e, "completed", time.Minute), productRequest(e, "request", time.Minute, 100))
	f.processingReceipts(t)
	f.now = f.now.Add(8 * 24 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	f.foldAll(t)
	check := func(want bool) {
		t.Helper()
		equal, err := f.metrics.Compare(ctx, f.actor, f.projectID, f.runID)
		if err != nil || equal != want {
			t.Fatal("retained parity", equal, err)
		}
	}
	check(true)
	var body []byte
	if err := f.pool.QueryRow(ctx, `SELECT historical_contribution FROM metric_user_state`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var changed derived
	if err := json.Unmarshal(body, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Requests[0].Count++
	bad, _ := json.Marshal(changed)
	if _, err := f.pool.Exec(ctx, `UPDATE metric_user_state SET historical_contribution=$1`, bad); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := f.pool.Exec(ctx, `UPDATE metric_user_state SET historical_contribution=$1`, body); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE metric_counts SET value=value+1 WHERE category='request' AND metric='count'`); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := f.pool.Exec(ctx, `UPDATE metric_counts SET value=value-1 WHERE category='request' AND metric='count'`); err != nil {
		t.Fatal(err)
	}
	check(true)
	// Reconcile a corrupted replaceable contribution/counter pair without
	// rebuilding or clearing older history: the retained source repairs its delta.
	newer := e
	newer.ID = "retained"
	newer.OccurredAt = f.now.Add(-time.Minute)
	f.ingest(t, newer, productRequest(newer, "retained_request", time.Second, 500))
	f.enqueueFacts(t)
	f.drain(t)
	check(true)
	if err := f.pool.QueryRow(ctx, `SELECT contribution FROM metric_user_state`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Requests[0].Count--
	bad, _ = json.Marshal(changed)
	if _, err := f.pool.Exec(ctx, `UPDATE metric_user_state SET contribution=$1,due_at=NULL`, bad); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE metric_counts SET value=value-1 WHERE category='request' AND metric='count'`); err != nil {
		t.Fatal(err)
	}
	check(false)
	f.enqueueFacts(t)
	f.drain(t)
	check(true)
}

func TestRetainedParityPagesAcrossMoreThanOneHundredUsers(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	facts := make([]events.Event, 105)
	for i := range facts {
		facts[i] = f.exposure(t, fmt.Sprintf("anchor_%03d", i), f.user(t, "control", i), 0)
	}
	f.ingest(t, facts[:100]...)
	f.ingest(t, facts[100:]...)
	f.processingReceipts(t)
	f.now = f.now.Add(8 * 24 * time.Hour)
	f.enqueueFacts(t)
	f.drain(t)
	for i := range 105 {
		r, err := FoldOne(ctx, f.pool, f.now)
		if err != nil || r.Raw != 1 {
			t.Fatal("user fold", i, r, err)
		}
		f.drain(t)
	}
	if equal, err := f.metrics.Compare(ctx, f.actor, f.projectID, f.runID); err != nil || !equal {
		t.Fatal("parity cursor lost a user", equal, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE metric_counts SET value=value-1 WHERE category='finalized' AND metric='exposed'`); err != nil {
		t.Fatal(err)
	}
	if equal, err := f.metrics.Compare(ctx, f.actor, f.projectID, f.runID); err != nil || equal {
		t.Fatal("parity ignored last-page dimensions", equal, err)
	}
}
