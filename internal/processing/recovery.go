package processing

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"switchyard/internal/audit"
	"switchyard/internal/auth"
	"switchyard/internal/metrics"
	"switchyard/internal/platform/identity"
	"switchyard/internal/platform/messaging"
)

const ReplayHorizon = 7 * 24 * time.Hour
const RecoveryBatch = 100

var ErrRecovery = errors.New("invalid or unavailable recovery operation")

// Recovery requires a current project administrator, even for a local CLI.
// These controls schedule existing facts; they cannot introduce/change facts.
type Operator struct {
	Actor  auth.Actor
	Reason string
}

func (o Operator) authorize(ctx context.Context, tx pgx.Tx, project, env string) error {
	if len(strings.TrimSpace(o.Reason)) < 1 || len(o.Reason) > 512 {
		return ErrRecovery
	}
	if err := auth.RequireRole(ctx, tx, o.Actor, "admin"); err != nil {
		return err
	}
	return auth.Authorize(ctx, tx, o.Actor, project, env, true)
}
func (o Operator) record(ctx context.Context, tx pgx.Tx, project, env, action string, details any) error {
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Entry{ActorID: o.Actor.ID, Source: "human", ProjectID: project, EnvironmentID: env, Action: action, RequestID: identity.New("recovery_"), Reason: o.Reason, Details: body})
}

type ReplayPage struct {
	Events int    `json:"events"`
	Cursor string `json:"cursor"`
	More   bool   `json:"more"`
}

// Replay processes one bounded page in stable event-ID order. The caller keeps
// the interval fixed across pages. Existing receipts/contributions are never
// cleared; reconciliation replaces contributions with differences atomically.
func (s *Store) Replay(ctx context.Context, o Operator, project, env, run string, from, until, now time.Time, cursor string) (ReplayPage, error) {
	var result ReplayPage
	if from.Before(now.Add(-s.replayHorizon)) || !from.Before(until) || until.After(now) || len(cursor) > 128 {
		return result, ErrRecovery
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	if err = o.authorize(ctx, tx, project, env); err != nil {
		return result, err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM experiment_runs WHERE project_id=$1 AND environment_id=$2 AND id=$3)`, project, env, run).Scan(&exists); err != nil {
		return result, err
	}
	if !exists {
		return result, ErrRecovery
	}
	rows, err := tx.Query(ctx, `SELECT event_id,user_id FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND received_at>=$4 AND received_at<$5 AND event_id COLLATE "C">$6 COLLATE "C" ORDER BY event_id COLLATE "C" LIMIT 101`, project, env, run, from, until, cursor)
	if err != nil {
		return result, err
	}
	type fact struct{ id, user string }
	facts := make([]fact, 0, RecoveryBatch+1)
	for rows.Next() {
		var f fact
		if err = rows.Scan(&f.id, &f.user); err != nil {
			rows.Close()
			return result, err
		}
		facts = append(facts, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	result.More = len(facts) > RecoveryBatch
	if result.More {
		facts = facts[:RecoveryBatch]
	}
	// Same lock order as normal processing; replay is serialized with reconciliation.
	for _, f := range facts {
		if err = metrics.EnqueueUser(ctx, tx, project, env, run, f.user); err != nil {
			return result, err
		}
		if err = metrics.EnqueueReferencingUsers(ctx, tx, project, env, f.id); err != nil {
			return result, err
		}
		result.Cursor = f.id
		result.Events++
	}
	if err = o.record(ctx, tx, project, env, "metrics.replay", struct {
		Run         string `json:"run_id"`
		From, Until time.Time
		ReplayPage
	}{run, from, until, result}); err != nil {
		return ReplayPage{}, err
	}
	return result, tx.Commit(ctx)
}

// RetryPublication resets only a dead, unleased publication whose source is
// retained. It preserves the original broker message ID and processing receipt.
func (s *Store) RetryPublication(ctx context.Context, o Operator, project, env string, id int64, now time.Time) error {
	if id < 1 {
		return ErrRecovery
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = o.authorize(ctx, tx, project, env); err != nil {
		return err
	}
	var kind, object string
	var revision int64
	var created time.Time
	err = tx.QueryRow(ctx, `SELECT kind,object_id,revision,created_at FROM outbox WHERE id=$1 AND project_id=$2 AND environment_id=$3 AND dead_at IS NOT NULL AND published_at IS NULL AND lease_until IS NULL FOR UPDATE`, id, project, env).Scan(&kind, &object, &revision, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRecovery
	}
	if err != nil {
		return err
	}
	if created.Before(now.Add(-ReplayHorizon)) || created.After(now) {
		return ErrRecovery
	}
	var exists bool
	if kind == "event" {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND event_id=$3 AND revision=$4 AND received_at>=$5 AND received_at<=$6)`, project, env, object, revision, now.Add(-ReplayHorizon), now).Scan(&exists)
	} else {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM flag_revisions WHERE project_id=$1 AND environment_id=$2 AND flag_id=$3 AND revision=$4)`, project, env, object, revision).Scan(&exists)
	}
	if err != nil {
		return err
	}
	if !exists {
		return ErrSourceMissing
	}
	if _, err = tx.Exec(ctx, `UPDATE outbox SET dead_at=NULL,attempts=0,failure_code='',available_at=clock_timestamp() WHERE id=$1`, id); err != nil {
		return err
	}
	if err = o.record(ctx, tx, project, env, "work.publication_retry", map[string]int64{"outbox_id": id}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RetryDeadLetter applies the stored envelope directly under the same validation
// as broker delivery. An invalid envelope stays inspectable; operators cannot
// edit its payload to bypass source validation. Resolution and audit are atomic
// with the receipt/scheduling transaction.
func (s *Store) RetryDeadLetter(ctx context.Context, o Operator, project, env, stream string, sequence int64, now time.Time) error {
	if sequence < 1 {
		return ErrRecovery
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if err = o.authorize(ctx, tx, project, env); err != nil {
		return err
	}
	var body []byte
	var id string
	var created time.Time
	err = tx.QueryRow(ctx, `SELECT payload,message_id,created_at FROM work_dead_letters WHERE stream_name=$1 AND stream_sequence=$2 AND resolved_at IS NULL FOR UPDATE`, stream, sequence).Scan(&body, &id, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRecovery
	}
	if err != nil {
		return err
	}
	if created.Before(now.Add(-ReplayHorizon)) || created.After(now) {
		return ErrRecovery
	}
	e, err := messaging.Decode(body)
	if err != nil {
		return err
	}
	if e.MessageID != id || e.Reference.ProjectID != project || e.Reference.EnvironmentID != env {
		return ErrIdentity
	}
	// A recent dead-letter cannot extend an old source's replay horizon.
	var sourceCreated time.Time
	if err = tx.QueryRow(ctx, `SELECT created_at FROM outbox WHERE 'switchyard-outbox-v1-'||id=$1`, id).Scan(&sourceCreated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSourceMissing
		}
		return err
	}
	if sourceCreated.Before(now.Add(-ReplayHorizon)) || sourceCreated.After(now) {
		return ErrRecovery
	}
	if e.Reference.Kind == "event" {
		var retained bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND event_id=$3 AND revision=$4 AND received_at>=$5 AND received_at<=$6)`, project, env, e.Reference.ObjectID, e.Reference.Revision, now.Add(-ReplayHorizon), now).Scan(&retained); err != nil {
			return err
		}
		if !retained {
			return ErrRecovery
		}
	}
	if _, err = applyTx(ctx, tx, e); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE work_dead_letters SET resolved_at=clock_timestamp() WHERE stream_name=$1 AND stream_sequence=$2`, stream, sequence); err != nil {
		return err
	}
	if err = o.record(ctx, tx, project, env, "work.dead_letter_retry", map[string]any{"stream": stream, "sequence": sequence, "message_id": id}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type Failure struct {
	Kind      string    `json:"kind"`
	ID        int64     `json:"id,omitempty"`
	Stream    string    `json:"stream,omitempty"`
	Sequence  int64     `json:"sequence,omitempty"`
	Code      string    `json:"code"`
	CreatedAt time.Time `json:"created_at"`
}

type FailurePage struct {
	Failures          []Failure `json:"failures"`
	PublicationCursor int64     `json:"publication_cursor"`
	StreamCursor      string    `json:"stream_cursor"`
	SequenceCursor    int64     `json:"sequence_cursor"`
	More              bool      `json:"more"`
}

func (s *Store) Inspect(ctx context.Context, actor auth.Actor, project, env string) ([]Failure, error) {
	page, err := s.InspectPage(ctx, actor, project, env, 0, "", 0)
	return page.Failures, err
}

// Cursors advance over scanned processing records even when their trusted scope
// does not match. An unrelated project cannot hide later failures behind a page.
func (s *Store) InspectPage(ctx context.Context, actor auth.Actor, project, env string, publication int64, stream string, sequence int64) (FailurePage, error) {
	page := FailurePage{Failures: make([]Failure, 0), PublicationCursor: publication, StreamCursor: stream, SequenceCursor: sequence}
	if publication < 0 || sequence < 0 || len(stream) > 128 {
		return page, ErrRecovery
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return page, err
	}
	defer tx.Rollback(context.Background())
	if err = auth.RequireRole(ctx, tx, actor, "admin"); err != nil {
		return page, err
	}
	if err = auth.Authorize(ctx, tx, actor, project, env, false); err != nil {
		return page, err
	}
	rows, err := tx.Query(ctx, `SELECT id,failure_code,dead_at FROM outbox WHERE project_id=$1 AND environment_id=$2 AND dead_at IS NOT NULL AND id>$3 ORDER BY id LIMIT 101`, project, env, publication)
	if err != nil {
		return page, err
	}
	scanned := 0
	for rows.Next() {
		var f Failure
		f.Kind = "publication"
		if err = rows.Scan(&f.ID, &f.Code, &f.CreatedAt); err != nil {
			rows.Close()
			return page, err
		}
		scanned++
		if scanned > RecoveryBatch {
			page.More = true
			continue
		}
		page.PublicationCursor = f.ID
		page.Failures = append(page.Failures, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	rows, err = tx.Query(ctx, `SELECT stream_name,stream_sequence,failure_code,created_at,payload FROM work_dead_letters WHERE resolved_at IS NULL AND (stream_name COLLATE "C",stream_sequence)>($1 COLLATE "C",$2::bigint) ORDER BY stream_name COLLATE "C",stream_sequence LIMIT 101`, stream, sequence)
	if err != nil {
		return page, err
	}
	scanned = 0
	for rows.Next() {
		var f Failure
		var body []byte
		f.Kind = "processing"
		if err = rows.Scan(&f.Stream, &f.Sequence, &f.Code, &f.CreatedAt, &body); err != nil {
			rows.Close()
			return page, err
		}
		scanned++
		if scanned > RecoveryBatch {
			page.More = true
			continue
		}
		page.StreamCursor = f.Stream
		page.SequenceCursor = f.Sequence
		e, decodeErr := messaging.Decode(body)
		if decodeErr != nil {
			f.Kind = "unscoped_invalid_envelope"
			page.Failures = append(page.Failures, f)
		} else if e.Reference.ProjectID == project && e.Reference.EnvironmentID == env {
			page.Failures = append(page.Failures, f)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	return page, tx.Commit(ctx)
}
