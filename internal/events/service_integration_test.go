//go:build integration

package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"switchyard/internal/auth"
	"switchyard/internal/events"
	"switchyard/internal/experiments"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
)

func TestIngestionIdentityQuarantineConcurrencyAndAtomicity(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Database(t)
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	actor := auth.Actor{ID: "admin"}
	ps := projects.New(pool)
	p, err := ps.Create(ctx, actor, "Event facts", "project")
	if err != nil {
		t.Fatal(err)
	}
	var dev, staging string
	if err = pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, p.ID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='staging'`, p.ID).Scan(&staging); err != nil {
		t.Fatal(err)
	}
	value := func(v string) evaluation.Value { return evaluation.Value{Type: "boolean", Data: json.RawMessage(v)} }
	cfg := flags.Configuration{Default: value("false"), Safe: value("false"), Rules: []evaluation.Rule{{Attribute: "country", Operator: "eq", Values: []json.RawMessage{json.RawMessage(`"JP"`)}, Value: value("true")}}}
	fs := flags.New(pool)
	if _, err = fs.Create(ctx, actor, p.ID, flags.CreateInput{EnvironmentID: dev, Key: "listing", Type: "boolean", Configuration: cfg, Reason: "baseline"}, "flag"); err != nil {
		t.Fatal(err)
	}
	xs := experiments.New(pool)
	run, err := xs.Create(ctx, actor, p.ID, experiments.CreateInput{EnvironmentID: dev, FlagKey: "listing", ExpectedRevision: 1, Name: "Listing", ControlVariantID: "control", TrafficBP: 10000, Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 5000, Value: value("false")}, {ID: "treatment", Ordinal: 1, WeightBP: 5000, Value: value("true")}}, Reason: "experiment"}, "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = xs.Transition(ctx, actor, p.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 1, Action: "start", Reason: "start"}, "start"); err != nil {
		t.Fatal(err)
	}
	d, err := fs.Get(ctx, p.ID, dev, "listing")
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := compiled.Evaluate("synthetic-user", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Historical revision validation must still work after the run is detached.
	if _, err = xs.Transition(ctx, actor, p.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 2, Action: "complete", Reason: "complete"}, "complete"); err != nil {
		t.Fatal(err)
	}
	key, err := ps.CreateKey(ctx, actor, p.ID, dev, "events", []string{"events:write"}, "key")
	if err != nil {
		t.Fatal(err)
	}
	evalKey, err := ps.CreateKey(ctx, actor, p.ID, dev, "evaluation", []string{"evaluate"}, "eval-key")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	current := now
	s := events.New(pool, func() time.Time { return current })
	exposure := events.Event{ID: "exposure_1", Kind: "exposure", RunID: run.ID, UserID: "synthetic-user", VariantID: decision.VariantID, Revision: 2, DecisionID: "dec_fixture", DecisionReason: "experiment", OccurredAt: now}
	ingest := func(token string, items ...events.Event) ([]events.Receipt, error) {
		return s.Ingest(ctx, token, events.Batch{ProjectID: p.ID, EnvironmentID: dev, Events: items})
	}
	completion := exposure
	completion.ID = "completion_1"
	completion.Kind = "listing_completion"
	completion.DecisionID = ""
	completion.ExposureID = exposure.ID
	completion.OccurredAt = now.Add(time.Minute)
	r, err := ingest(key.Token, completion)
	if err != nil || r[0].Status != "accepted" {
		t.Fatalf("outcome before exposure: %+v %v", r, err)
	}
	r, err = ingest(key.Token, exposure)
	if err != nil || r[0].Duplicate || r[0].Status != "accepted" {
		t.Fatalf("exposure: %+v %v", r, err)
	}
	equivalent := exposure
	equivalent.OccurredAt = now.In(time.FixedZone("offset", 3600))
	equivalent.Attributes = map[string]json.RawMessage{}
	r, err = ingest(key.Token, equivalent)
	if err != nil || !r[0].Duplicate {
		t.Fatal("equivalent retry not deduplicated")
	}
	changed := exposure
	changed.UserID = "different-user"
	first := exposure
	first.ID = "a_before_conflict"
	if _, err = ingest(key.Token, first, changed); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("payload conflict: %v", err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM raw_events WHERE event_id='a_before_conflict'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("conflicted batch partially committed")
	}
	if _, err = ingest(evalKey.Token, exposure); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("evaluation key accepted event")
	}
	if _, err = s.Ingest(ctx, key.Token, events.Batch{ProjectID: p.ID, EnvironmentID: staging, Events: []events.Event{exposure}}); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("cross-environment accepted")
	}
	wrong := exposure
	wrong.ID = "wrong_variant"
	wrong.VariantID = "invalid"
	targeted := exposure
	targeted.ID = "targeted"
	targeted.Attributes = map[string]json.RawMessage{"country": json.RawMessage(`"JP"`)}
	targeted.DecisionReason = "targeting"
	targeted.VariantID = ""
	late := exposure
	late.ID = "late"
	late.OccurredAt = now.Add(-24*time.Hour - time.Microsecond)
	future := exposure
	future.ID = "future"
	future.OccurredAt = now.Add(5*time.Minute + time.Microsecond)
	unknownRevision := exposure
	unknownRevision.ID = "unknown_revision"
	unknownRevision.Revision = 999
	draftRevision := exposure
	draftRevision.ID = "draft_revision"
	draftRevision.Revision = 1
	r, err = ingest(key.Token, wrong, targeted, late, future, unknownRevision, draftRevision)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{"assignment_mismatch", "non_randomized_exposure", "too_late", "future_timestamp", "unknown_revision", "revision_run_mismatch"}
	for i, receipt := range r {
		if receipt.Status != "quarantined" || receipt.Reason != expected[i] {
			t.Fatalf("quarantine order %d %+v", i, receipt)
		}
	}
	// Retrying later returns the original receipt, rather than changing accepted history.
	current = now.Add(25 * time.Hour)
	r, err = ingest(key.Token, exposure)
	if err != nil || r[0].Status != "accepted" || !r[0].Duplicate {
		t.Fatal("retry reclassified historical receipt")
	}
	current = now
	numeric := exposure
	numeric.ID = "numeric"
	numeric.Attributes = map[string]json.RawMessage{"score": json.RawMessage(`200.0`)}
	if _, err = ingest(key.Token, numeric); err != nil {
		t.Fatal(err)
	}
	numeric.Attributes = map[string]json.RawMessage{"score": json.RawMessage(`2e2`)}
	r, err = ingest(key.Token, numeric)
	if err != nil || !r[0].Duplicate {
		t.Fatal("equivalent numeric payload conflicted")
	}
	// Concurrent reversed batches acquire identities in the same order.
	a, b := exposure, exposure
	a.ID = "concurrent_a"
	b.ID = "concurrent_b"
	var wg sync.WaitGroup
	receipts := make(chan []events.Receipt, 2)
	errs := make(chan error, 2)
	for _, items := range [][]events.Event{{a, b}, {b, a}} {
		wg.Go(func() { r, err := ingest(key.Token, items...); receipts <- r; errs <- err })
	}
	wg.Wait()
	close(receipts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	duplicates := 0
	for batch := range receipts {
		for _, receipt := range batch {
			if receipt.Duplicate {
				duplicates++
			}
		}
	}
	if duplicates != 2 {
		t.Fatalf("concurrent duplicates %d", duplicates)
	}
	if _, err = pool.Exec(ctx, `CREATE FUNCTION reject_fixture_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_id='z_reject' THEN RAISE EXCEPTION 'fixture rejection'; END IF; RETURN NEW; END; $$; CREATE TRIGGER fixture_rejection BEFORE INSERT ON raw_events FOR EACH ROW EXECUTE FUNCTION reject_fixture_event()`); err != nil {
		t.Fatal(err)
	}
	a.ID = "a_atomic"
	b.ID = "z_reject"
	if receipts, err := ingest(key.Token, a, b); err == nil || receipts != nil {
		t.Fatal("failed transaction returned acknowledgement")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM raw_events WHERE event_id='a_atomic'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed insert partially committed")
	}
	// Deferred rejection happens at COMMIT, after all INSERT statements succeeded.
	if _, err = pool.Exec(ctx, `CREATE FUNCTION reject_fixture_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_id='commit_reject' THEN RAISE EXCEPTION 'deferred fixture rejection'; END IF; RETURN NEW; END; $$; CREATE CONSTRAINT TRIGGER fixture_commit_rejection AFTER INSERT ON raw_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_fixture_commit()`); err != nil {
		t.Fatal(err)
	}
	a.ID = "a_commit_atomic"
	b.ID = "commit_reject"
	if receipts, err := ingest(key.Token, a, b); err == nil || receipts != nil {
		t.Fatal("failed COMMIT returned acknowledgement")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM raw_events WHERE event_id IN ('a_commit_atomic','commit_reject')`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed COMMIT retained raw facts")
	}
	if _, err = pool.Exec(ctx, `UPDATE raw_events SET variant_id='rewritten'`); err == nil {
		t.Fatal("raw fact mutable")
	}
	var rawCount, intentCount int
	if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM raw_events),(SELECT count(*) FROM outbox WHERE kind='event')`).Scan(&rawCount, &intentCount); err != nil || rawCount != intentCount {
		t.Fatalf("raw/publication parity=%d/%d %v", rawCount, intentCount, err)
	}
	if _, err = pool.Exec(ctx, `CREATE FUNCTION reject_outbox_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outbox fixture rejection'; END; $$; CREATE TRIGGER outbox_fixture BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION reject_outbox_fixture()`); err != nil {
		t.Fatal(err)
	}
	a.ID = "outbox_rejected"
	if receipts, err := ingest(key.Token, a); err == nil || receipts != nil {
		t.Fatal("fact acknowledged without publication intent")
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM raw_events WHERE event_id='outbox_rejected'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("outbox failure retained fact")
	}
	if err = ps.RevokeKey(ctx, actor, p.ID, key.ID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err = ingest(key.Token, exposure); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("revoked key accepted event")
	}
}
