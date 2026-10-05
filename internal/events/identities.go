package events

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/auth"
)

const IdentityRetention = 8 * 24 * time.Hour

// Raw wins during the folding transaction's overlap. Archive lookups preserve
// the original accepted/quarantined receipt and semantic numeric/JSON equality.
const receiptSQL = `SELECT payload=$4::jsonb,status,quarantine_reason FROM (
 SELECT payload,status,quarantine_reason,0 AS priority FROM raw_events
 WHERE project_id=$1 AND environment_id=$2 AND event_id=$3
 UNION ALL SELECT payload,status,quarantine_reason,1 AS priority FROM retained_event_identities
 WHERE project_id=$1 AND environment_id=$2 AND event_id=$3
) identities ORDER BY priority LIMIT 1`

// PreserveIdentities is a bounded primitive for the raw-folding transaction.
// The caller locks/validates its chosen facts and commits identity preservation
// with summary folding and raw deletion. It cannot overwrite an old identity.
func PreserveIdentities(ctx context.Context, tx pgx.Tx, project, env string, ids []string) (int64, error) {
	if tx == nil || len(ids) < 1 || len(ids) > MaxBatchSize {
		return 0, auth.ErrInvalid
	}
	for _, id := range ids {
		if !eventIDPattern.MatchString(id) {
			return 0, auth.ErrInvalid
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO retained_event_identities(project_id,environment_id,event_id,payload,status,quarantine_reason,received_at,expires_at)
 SELECT project_id,environment_id,event_id,payload,status,quarantine_reason,received_at,received_at+interval '192 hours'
 FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND event_id=ANY($3::text[])
 ORDER BY event_id COLLATE "C" ON CONFLICT(project_id,environment_id,event_id) DO NOTHING`, project, env, ids)
	return tag.RowsAffected(), err
}

// Expiry is based on original receipt time, never on retry or folding time.
// This function deletes at most 100 records and is not yet scheduled by worker.
func PruneIdentities(ctx context.Context, pool *pgxpool.Pool, now time.Time, limit int) (int64, error) {
	if limit < 1 || limit > MaxBatchSize {
		return 0, auth.ErrInvalid
	}
	tag, err := pool.Exec(ctx, `WITH expired AS (
 SELECT project_id,environment_id,event_id FROM retained_event_identities WHERE expires_at<=$1
 ORDER BY expires_at,project_id,environment_id,event_id LIMIT $2 FOR UPDATE SKIP LOCKED)
 DELETE FROM retained_event_identities a USING expired e WHERE a.project_id=e.project_id
 AND a.environment_id=e.environment_id AND a.event_id=e.event_id`, now.UTC().Truncate(time.Microsecond), limit)
	return tag.RowsAffected(), err
}
