// Package processing commits durable reconciliation intent before broker ack.
package processing

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/metrics"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/messaging"
)

var ErrIdentity = errors.New("work identity mismatch")
var ErrSourceMissing = errors.New("work source missing")

type Store struct {
	pool          *pgxpool.Pool
	replayHorizon time.Duration
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool, replayHorizon: ReplayHorizon} }

// NewWithRawRetention aligns raw replay with the configured cleanup horizon.
func NewWithRawRetention(pool *pgxpool.Pool, days int) (*Store, error) {
	if days < 2 || days > 7 {
		return nil, ErrRecovery
	}
	s := New(pool)
	s.replayHorizon = time.Duration(days) * 24 * time.Hour
	return s, nil
}

// Apply validates both the persisted publication intent and source fact. A
// duplicate receipt remains valid after raw retention, but cannot change its
// original reference. Missing new source work is inspectable, never applied.
func (s *Store) Apply(ctx context.Context, e messaging.Envelope) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	duplicate, err := applyTx(ctx, tx, e)
	if err != nil {
		return false, err
	}
	return duplicate, tx.Commit(ctx)
}

func applyTx(ctx context.Context, tx pgx.Tx, e messaging.Envelope) (bool, error) {
	if e.Version != 1 || !e.Reference.Valid() {
		return false, ErrIdentity
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(e.MessageID, "switchyard-outbox-v1-"), 10, 64)
	if err != nil || id < 1 || e.MessageID != (outbox.Item{ID: id}).MessageID() {
		return false, ErrIdentity
	}
	body, err := json.Marshal(e.Reference)
	if err != nil {
		return false, err
	}
	var equal bool
	err = tx.QueryRow(ctx, `SELECT reference=$2::jsonb FROM processed_work WHERE message_id=$1`, e.MessageID, body).Scan(&equal)
	if err == nil {
		if !equal {
			return false, ErrIdentity
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	var r outbox.Reference
	err = tx.QueryRow(ctx, `SELECT kind,project_id,environment_id,object_id,revision FROM outbox WHERE id=$1 FOR KEY SHARE`, id).Scan(&r.Kind, &r.ProjectID, &r.EnvironmentID, &r.ObjectID, &r.Revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrSourceMissing
	}
	if err != nil {
		return false, err
	}
	if r != e.Reference {
		return false, ErrIdentity
	}
	// Receipt uniqueness serializes simultaneous redelivery before any work is
	// enqueued. Transaction failure rolls this receipt back with the work.
	tag, err := tx.Exec(ctx, `INSERT INTO processed_work(message_id,reference) VALUES($1,$2) ON CONFLICT DO NOTHING`, e.MessageID, body)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		if err = tx.QueryRow(ctx, `SELECT reference=$2::jsonb FROM processed_work WHERE message_id=$1`, e.MessageID, body).Scan(&equal); err != nil {
			return false, err
		}
		if !equal {
			return false, ErrIdentity
		}
		return true, nil
	}
	if r.Kind == "event" {
		var run, user string
		err = tx.QueryRow(ctx, `SELECT run_id,user_id FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND event_id=$3 AND revision=$4`, r.ProjectID, r.EnvironmentID, r.ObjectID, r.Revision).Scan(&run, &user)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrSourceMissing
		}
		if err != nil {
			return false, err
		}
		if err = metrics.EnqueueUser(ctx, tx, r.ProjectID, r.EnvironmentID, run, user); err != nil {
			return false, err
		}
		if err = metrics.EnqueueReferencingUsers(ctx, tx, r.ProjectID, r.EnvironmentID, r.ObjectID); err != nil {
			return false, err
		}
	} else {
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM flag_revisions WHERE project_id=$1 AND environment_id=$2 AND flag_id=$3 AND revision=$4)`, r.ProjectID, r.EnvironmentID, r.ObjectID, r.Revision).Scan(&equal); err != nil {
			return false, err
		}
		if !equal {
			return false, ErrSourceMissing
		}
	}
	return false, nil
}
func (s *Store) DeadLetter(ctx context.Context, stream string, sequence uint64, id string, payload []byte, code string) error {
	if len(stream) < 1 || len(stream) > 128 || sequence == 0 || sequence > uint64(^uint64(0)>>1) || len(id) > 128 || len(payload) > messaging.MaxEnvelopeBytes ||
		(code != "invalid_envelope" && code != "identity_mismatch" && code != "source_missing") {
		return ErrIdentity
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO work_dead_letters(stream_name,stream_sequence,message_id,payload,failure_code)
        VALUES($1,$2,$3,$4,$5) ON CONFLICT(stream_name,stream_sequence) DO NOTHING`, stream, int64(sequence), id, payload, code)
	return outbox.CapacityError(err)
}
