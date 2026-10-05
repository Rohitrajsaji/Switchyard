// Package outbox records durable publication intent inside domain transactions.
package outbox

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/platform/identity"
	"switchyard/internal/platform/telemetry"
)

const MaxBatch = 100
const MaxAttempts = 10

var ErrInvalid = errors.New("invalid outbox operation")
var ErrClaimLost = errors.New("outbox claim lost")
var ErrCapacity = errors.New("durable work capacity reached")

// CapacityError uses a dedicated database code, never exception-text matching.
func CapacityError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "SY001" {
		return ErrCapacity
	}
	return err
}

type Reference struct {
	Kind          string `json:"kind"`
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	ObjectID      string `json:"object_id"`
	Revision      int64  `json:"revision"`
}

func (r Reference) Valid() bool {
	return (r.Kind == "event" || r.Kind == "configuration") && len(r.ProjectID) > 0 && len(r.ProjectID) <= 128 &&
		len(r.EnvironmentID) > 0 && len(r.EnvironmentID) <= 128 && len(r.ObjectID) > 0 && len(r.ObjectID) <= 128 && r.Revision > 0
}

// Record receives the existing transaction; it never commits independently.
func Record(ctx context.Context, tx pgx.Tx, r Reference) error {
	if tx == nil || !r.Valid() {
		return ErrInvalid
	}
	_, err := tx.Exec(ctx, `INSERT INTO outbox(kind,project_id,environment_id,object_id,revision,traceparent)
        VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(kind,project_id,environment_id,object_id,revision) DO NOTHING`,
		r.Kind, r.ProjectID, r.EnvironmentID, r.ObjectID, r.Revision, telemetry.Traceparent(ctx))
	return CapacityError(err)
}

type Item struct {
	ID int64
	Reference
	ClaimToken string
	Attempts   int
	// Traceparent is the originating request's W3C trace context, or "".
	Traceparent string
}

// MessageID remains the same through ambiguous acknowledgements and retries.
func (i Item) MessageID() string { return "switchyard-outbox-v1-" + strconv.FormatInt(i.ID, 10) }

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Claim is a short database transaction. Network publication holds no row lock.
// SKIP LOCKED permits another publisher to claim disjoint work; a random claim
// token fences completion by a publisher whose lease was replaced.
func (s *Store) Claim(ctx context.Context, limit int, lease time.Duration) ([]Item, error) {
	if limit < 1 || limit > MaxBatch || lease < time.Second || lease > time.Minute {
		return nil, ErrInvalid
	}
	token := identity.New("claim_")
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	// A crash on the final attempt cannot leave a permanently unclaimable row.
	if _, err = tx.Exec(ctx, `WITH exhausted AS (
        SELECT id FROM outbox WHERE published_at IS NULL AND dead_at IS NULL AND attempts >= 10
        AND (lease_until IS NULL OR lease_until <= clock_timestamp()) ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED)
        UPDATE outbox SET dead_at=clock_timestamp(),claim_token=NULL,lease_until=NULL,failure_code='attempts_exhausted'
        WHERE id IN (SELECT id FROM exhausted)`, limit); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `WITH pending AS (
        SELECT id FROM outbox WHERE published_at IS NULL AND dead_at IS NULL AND attempts < 10
        AND available_at <= clock_timestamp() AND (lease_until IS NULL OR lease_until <= clock_timestamp())
        ORDER BY available_at,id LIMIT $1 FOR UPDATE SKIP LOCKED)
        UPDATE outbox SET claim_token=$2,lease_until=clock_timestamp()+$3::bigint*interval '1 millisecond',attempts=attempts+1
        WHERE id IN (SELECT id FROM pending)
        RETURNING id,kind,project_id,environment_id,object_id,revision,claim_token,attempts,traceparent`, limit, token, lease.Milliseconds())
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, limit)
	for rows.Next() {
		var item Item
		if err = rows.Scan(&item.ID, &item.Kind, &item.ProjectID, &item.EnvironmentID, &item.ObjectID, &item.Revision, &item.ClaimToken, &item.Attempts, &item.Traceparent); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}
func (s *Store) Published(ctx context.Context, i Item) error {
	tag, err := s.pool.Exec(ctx, `UPDATE outbox SET published_at=clock_timestamp(),claim_token=NULL,lease_until=NULL,failure_code=''
        WHERE id=$1 AND claim_token=$2 AND published_at IS NULL AND dead_at IS NULL AND lease_until>clock_timestamp()`, i.ID, i.ClaimToken)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}
func (s *Store) Failed(ctx context.Context, i Item) error {
	delay := time.Second << min(max(i.Attempts-1, 0), 6)
	tag, err := s.pool.Exec(ctx, `UPDATE outbox SET claim_token=NULL,lease_until=NULL,
        available_at=clock_timestamp()+$3::bigint*interval '1 millisecond',
        dead_at=CASE WHEN attempts>=10 THEN clock_timestamp() ELSE NULL END,
        failure_code=CASE WHEN attempts>=10 THEN 'attempts_exhausted' ELSE 'publish_failed' END
        WHERE id=$1 AND claim_token=$2 AND published_at IS NULL AND dead_at IS NULL AND lease_until>clock_timestamp()`, i.ID, i.ClaimToken, delay.Milliseconds())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrClaimLost
	}
	return nil
}
