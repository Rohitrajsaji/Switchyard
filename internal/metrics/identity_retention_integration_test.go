//go:build integration

package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"switchyard/internal/auth"
	"switchyard/internal/events"
	"sync"
	"testing"
	"time"
)

func TestFoldedIdentityPreservesReceiptAndRejectsExpiredReplay(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	e := f.exposure(t, "retained_identity", f.user(t, "control", 0), 0)
	e.Attributes = map[string]json.RawMessage{"amount": json.RawMessage(`1.00`)}
	f.ingest(t, e)
	receiptTime := f.now
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	n, err := events.PreserveIdentities(ctx, tx, f.projectID, f.env, []string{e.ID})
	if err != nil || n != 1 {
		t.Fatal("identity not preserved", n, err)
	}
	if n, err = events.PreserveIdentities(ctx, tx, f.projectID, f.env, []string{e.ID}); err != nil || n != 0 {
		t.Fatal("identity duplicated", n, err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM raw_events WHERE event_id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.now = receiptTime.Add(7*24*time.Hour + time.Hour)
	equivalent := e
	equivalent.OccurredAt = e.OccurredAt.In(time.FixedZone("alternate", 9*3600))
	equivalent.Attributes = map[string]json.RawMessage{"amount": json.RawMessage(`1e0`)}
	ingest := func(event events.Event) ([]events.Receipt, error) {
		return f.events.Ingest(ctx, f.token, events.Batch{ProjectID: f.projectID, EnvironmentID: f.env, Events: []events.Event{event}})
	}
	var g sync.WaitGroup
	for range 20 {
		g.Go(func() {
			r, err := ingest(equivalent)
			if err != nil || len(r) != 1 || !r[0].Duplicate || r[0].Status != "accepted" {
				t.Errorf("lost archived receipt %v %v", r, err)
			}
		})
	}
	g.Wait()
	altered := e
	altered.UserID = f.user(t, "control", 1)
	if _, err = ingest(altered); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("archived identity payload changed", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE retained_event_identities SET expires_at=expires_at+interval '1 day'`); err == nil {
		t.Fatal("identity expiry extended by rewrite")
	}
	var raw, intents, identities int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM raw_events),(SELECT count(*) FROM outbox WHERE kind='event'),(SELECT count(*) FROM retained_event_identities)`).Scan(&raw, &intents, &identities); err != nil || raw != 0 || intents != 1 || identities != 1 {
		t.Fatal("archived retries created facts/intents")
	}
	expiry := receiptTime.Add(events.IdentityRetention)
	if n, err = events.PruneIdentities(ctx, f.pool, expiry.Add(-time.Microsecond), 1); err != nil || n != 0 {
		t.Fatal("identity expired early", n, err)
	}
	if n, err = events.PruneIdentities(ctx, f.pool, expiry, 1); err != nil || n != 1 {
		t.Fatal("identity boundary not expired", n, err)
	}
	f.now = expiry
	if r, err := ingest(e); !errors.Is(err, auth.ErrInvalid) || r != nil {
		t.Fatal("expired replay created a new receipt", r, err)
	}
	fresh := e
	fresh.OccurredAt = f.now
	if r, err := ingest(fresh); !errors.Is(err, auth.ErrConflict) || r != nil {
		t.Fatal("fresh timestamp bypassed retained identity references", r, err)
	}
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM raw_events`).Scan(&raw); err != nil || raw != 0 {
		t.Fatal("expired replay created raw fact")
	}
}

func TestIdentityPreservationAndDeletionRollbackTogether(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	e := f.exposure(t, "atomic_identity", f.user(t, "control", 0), 0)
	f.ingest(t, e)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = events.PreserveIdentities(ctx, tx, f.projectID, f.env, []string{e.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM raw_events WHERE event_id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var raw, archive int
	if err = f.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM raw_events),(SELECT count(*) FROM retained_event_identities)`).Scan(&raw, &archive); err != nil || raw != 1 || archive != 0 {
		t.Fatal("failed folding left partial identity/raw state")
	}
}

func TestArchivedQuarantineReceiptCannotBecomeAccepted(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	// This eight-day interval crosses the November DST change. Calendar-day
	// arithmetic in New York would differ from the contract's exact duration.
	f.base = time.Date(2026, time.October, 31, 0, 0, 0, 0, time.UTC)
	f.now = f.base.Add(2 * time.Hour)
	e := f.exposure(t, "quarantined_identity", f.user(t, "control", 0), 2*time.Hour+events.FutureAllowance+time.Microsecond)
	ingest := func() ([]events.Receipt, error) {
		return f.events.Ingest(ctx, f.token, events.Batch{ProjectID: f.projectID, EnvironmentID: f.env, Events: []events.Event{e}})
	}
	original, err := ingest()
	if err != nil || len(original) != 1 || original[0].Status != "quarantined" || original[0].Reason != "future_timestamp" {
		t.Fatal("fixture not quarantined", original, err)
	}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SET LOCAL TIME ZONE 'America/New_York'`); err != nil {
		t.Fatal(err)
	}
	if _, err = events.PreserveIdentities(ctx, tx, f.projectID, f.env, []string{e.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM raw_events WHERE event_id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(7*24*time.Hour + time.Hour)
	retry, err := ingest()
	if err != nil || len(retry) != 1 || !retry[0].Duplicate || retry[0].Status != original[0].Status || retry[0].Reason != original[0].Reason {
		t.Fatal("quarantine receipt changed after folding", retry, err)
	}
	var duration time.Duration
	var seconds int64
	if err = f.pool.QueryRow(ctx, `SELECT extract(epoch FROM expires_at-received_at)::bigint FROM retained_event_identities`).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	duration = time.Duration(seconds) * time.Second
	if duration != events.IdentityRetention {
		t.Fatal("identity duration depends on session timezone")
	}
}

func TestIdentityExpiryDeletesOnlyBoundedPages(t *testing.T) {
	f := setupMeasurement(t)
	f.compareAggregates = false
	ctx := context.Background()
	batch := make([]events.Event, 100)
	ids := make([]string, 100)
	user := f.user(t, "control", 0)
	for i := range batch {
		ids[i] = fmt.Sprintf("expiry_%03d", i)
		batch[i] = f.exposure(t, ids[i], user, 0)
	}
	f.ingest(t, batch...)
	last := f.exposure(t, "expiry_last", user, 0)
	f.ingest(t, last)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if n, err := events.PreserveIdentities(ctx, tx, f.projectID, f.env, ids); err != nil || n != 100 {
		t.Fatal("identity batch preservation", n, err)
	}
	if _, err = events.PreserveIdentities(ctx, tx, f.projectID, f.env, []string{last.ID}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	now := f.now.Add(events.IdentityRetention)
	if _, err = events.PruneIdentities(ctx, f.pool, now, 101); !errors.Is(err, auth.ErrInvalid) {
		t.Fatal("unbounded expiry admitted")
	}
	if n, err := events.PruneIdentities(ctx, f.pool, now, 100); err != nil || n != 100 {
		t.Fatal("expiry did not honor page limit", n, err)
	}
	var left int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM retained_event_identities`).Scan(&left); err != nil || left != 1 {
		t.Fatal("expiry deleted beyond its page")
	}
	if n, err := events.PruneIdentities(ctx, f.pool, now, 100); err != nil || n != 1 {
		t.Fatal("expiry next page lost", n, err)
	}
}
