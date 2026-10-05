//go:build integration

package processing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"switchyard/internal/auth"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/messaging"
)

func operator(t *testing.T, s *Store) Operator {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO project_memberships(project_id,user_id) VALUES('project','admin')`); err != nil {
		t.Fatal(err)
	}
	return Operator{Actor: auth.Actor{ID: "admin"}, Reason: "Repair local fixture"}
}
func TestReplayPagesPreserveReceiptsAndRejectExpiredScope(t *testing.T) {
	p, s := setup(t)
	o := operator(t, s)
	ctx := context.Background()
	for i := range 105 {
		e := fact(t, p, fmt.Sprintf("display-%03d", i), fmt.Sprintf("user-%03d", i))
		if _, err := s.Apply(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	start, end := now.Add(-time.Hour), now
	first, err := s.Replay(ctx, o, "project", "dev", "run", start, end, now, "")
	if err != nil || first.Events != 100 || !first.More || first.Cursor != "display-099" {
		t.Fatalf("first=%+v %v", first, err)
	}
	second, err := s.Replay(ctx, o, "project", "dev", "run", start, end, now, first.Cursor)
	if err != nil || second.Events != 5 || second.More || second.Cursor != "display-104" {
		t.Fatalf("second=%+v %v", second, err)
	}
	var receipts, states, audits int
	if err = p.QueryRow(ctx, `SELECT (SELECT count(*) FROM processed_work),(SELECT count(*) FROM metric_user_state),(SELECT count(*) FROM audit_entries WHERE action='metrics.replay')`).Scan(&receipts, &states, &audits); err != nil || receipts != 105 || states != 105 || audits != 2 {
		t.Fatalf("counts %d %d %d %v", receipts, states, audits, err)
	}
	if _, err = s.Replay(ctx, o, "project", "dev", "run", now.Add(-ReplayHorizon-time.Microsecond), end, now, ""); !errors.Is(err, ErrRecovery) {
		t.Fatal("expired replay accepted")
	}
	if _, err = s.Replay(ctx, o, "project", "dev", "run", start, end.Add(time.Second), now, ""); !errors.Is(err, ErrRecovery) {
		t.Fatal("future replay accepted")
	}
	forged := o
	forged.Actor = auth.Actor{ID: "unknown", Role: "admin"}
	if _, err = s.Replay(ctx, forged, "project", "dev", "run", start, end, now, ""); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("forged operator accepted")
	}
	if _, err = s.Replay(ctx, o, "other", "dev", "run", start, end, now, ""); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("foreign project accepted")
	}
}

func TestDeadLetterRepairAndAuditCommitTogether(t *testing.T) {
	p, s := setup(t)
	o := operator(t, s)
	ctx := context.Background()
	e := fact(t, p, "display", "user")
	var id int64
	if err := p.QueryRow(ctx, `SELECT id FROM outbox WHERE object_id='display'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	body, err := messaging.Encode(outbox.Item{ID: id, Reference: e.Reference})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeadLetter(ctx, "STREAM", 1, e.MessageID, body, "source_missing"); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Exec(ctx, `CREATE FUNCTION reject_recovery_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit fixture failure'; END; $$; CREATE TRIGGER recovery_fixture BEFORE INSERT ON audit_entries FOR EACH ROW EXECUTE FUNCTION reject_recovery_audit()`); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryDeadLetter(ctx, o, "project", "dev", "STREAM", 1, time.Now()); err == nil {
		t.Fatal("audit failure accepted")
	}
	var receipts, states, resolved int
	if err = p.QueryRow(ctx, `SELECT (SELECT count(*) FROM processed_work),(SELECT count(*) FROM metric_user_state),(SELECT count(*) FROM work_dead_letters WHERE resolved_at IS NOT NULL)`).Scan(&receipts, &states, &resolved); err != nil || receipts != 0 || states != 0 || resolved != 0 {
		t.Fatal("failed audit committed processing")
	}
	if _, err = p.Exec(ctx, `DROP TRIGGER recovery_fixture ON audit_entries`); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryDeadLetter(ctx, o, "project", "dev", "STREAM", 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryDeadLetter(ctx, o, "project", "dev", "STREAM", 1, time.Now()); !errors.Is(err, ErrRecovery) {
		t.Fatal("resolved record retried")
	}
	if duplicate, err := s.Apply(ctx, e); err != nil || !duplicate {
		t.Fatal("operator repair lost receipt")
	}
	if err = s.DeadLetter(ctx, "STREAM", 2, "invalid", []byte{0xff, 0x00}, "invalid_envelope"); err != nil {
		t.Fatal(err)
	}
	failures, err := s.Inspect(ctx, o.Actor, "project", "dev")
	if err != nil || len(failures) != 1 || failures[0].Kind != "unscoped_invalid_envelope" {
		t.Fatalf("inspect=%+v %v", failures, err)
	}
	if err = s.RetryDeadLetter(ctx, o, "project", "dev", "STREAM", 2, time.Now()); !errors.Is(err, messaging.ErrEnvelope) {
		t.Fatal("malformed payload repaired")
	}
}

func TestDeadPublicationRetryKeepsIdentityAndRejectsMissingOldSources(t *testing.T) {
	p, s := setup(t)
	o := operator(t, s)
	ctx := context.Background()
	e := fact(t, p, "display", "user")
	var id int64
	if err := p.QueryRow(ctx, `UPDATE outbox SET attempts=10,dead_at=clock_timestamp(),failure_code='attempts_exhausted' WHERE object_id='display' RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryPublication(ctx, o, "project", "dev", id, time.Now()); err != nil {
		t.Fatal(err)
	}
	items, err := outbox.New(p).Claim(ctx, 1, time.Second)
	if err != nil || len(items) != 1 || items[0].Attempts != 1 || items[0].MessageID() != e.MessageID {
		t.Fatal("retry changed identity", err)
	}
	if err = s.RetryPublication(ctx, o, "project", "dev", id, time.Now()); !errors.Is(err, ErrRecovery) {
		t.Fatal("active lease reset")
	}
	if _, err = p.Exec(ctx, `UPDATE outbox SET claim_token=NULL,lease_until=NULL,dead_at=clock_timestamp(),attempts=10 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryPublication(ctx, o, "project", "dev", id, time.Now().Add(ReplayHorizon+time.Hour)); !errors.Is(err, ErrRecovery) {
		t.Fatal("expired retry accepted")
	}
	if _, err = p.Exec(ctx, `DELETE FROM raw_events WHERE event_id='display'`); err != nil {
		t.Fatal(err)
	}
	if err = s.RetryPublication(ctx, o, "project", "dev", id, time.Now()); !errors.Is(err, ErrSourceMissing) {
		t.Fatal("missing source retried")
	}
}

func TestFailureInspectionCursorCrossesUnrelatedScopes(t *testing.T) {
	p, s := setup(t)
	o := operator(t, s)
	ctx := context.Background()
	e := fact(t, p, "display", "user")
	for i := 1; i <= 101; i++ {
		scope := e.Reference
		if i <= 100 {
			scope.ProjectID = "other"
		}
		body, err := messaging.Encode(outbox.Item{ID: int64(i), Reference: scope})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.DeadLetter(ctx, "STREAM", uint64(i), (outbox.Item{ID: int64(i)}).MessageID(), body, "source_missing"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.InspectPage(ctx, o.Actor, "project", "dev", 0, "", 0)
	if err != nil || len(first.Failures) != 0 || !first.More || first.SequenceCursor != 100 {
		t.Fatalf("first=%+v %v", first, err)
	}
	second, err := s.InspectPage(ctx, o.Actor, "project", "dev", first.PublicationCursor, first.StreamCursor, first.SequenceCursor)
	if err != nil || len(second.Failures) != 1 || second.More || second.SequenceCursor != 101 {
		t.Fatalf("second=%+v %v", second, err)
	}
}

func TestConfiguredRawReplayRejectsIntervalsOutsideCleanupHorizon(t *testing.T) {
	p, _ := setup(t)
	now := time.Now().UTC()
	store, err := NewWithRawRetention(p, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replay(context.Background(), Operator{}, "project", "dev", "run", now.Add(-3*24*time.Hour), now, now, ""); !errors.Is(err, ErrRecovery) {
		t.Fatal("replay exceeded configured raw horizon", err)
	}
	for _, days := range []int{1, 8} {
		if _, err := NewWithRawRetention(p, days); !errors.Is(err, ErrRecovery) {
			t.Fatal("unsafe raw replay horizon")
		}
	}
}
