package processing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const ReceiptRetention = 8 * 24 * time.Hour

type PrunedWork struct {
	Publications     int64
	Receipts         int64
	ResolvedFailures int64
}

// PruneCompleted removes at most limit completed publications and limit resolved
// failures. Receipt/source removal is atomic. Raw facts, pending publications,
// leases and unresolved failures are preserved. Automatic scheduling is gated
// on the remaining M7 summary expiry and recovery checks.
func PruneCompleted(ctx context.Context, pool *pgxpool.Pool, now time.Time, limit int) (PrunedWork, error) {
	var result PrunedWork
	if limit < 1 || limit > 100 || now.IsZero() {
		return result, errors.New("invalid work retention bound")
	}
	cutoff := now.UTC().Truncate(time.Microsecond).Add(-ReceiptRetention)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx, `SELECT o.id FROM outbox o JOIN processed_work w ON w.message_id='switchyard-outbox-v1-'||o.id
 WHERE o.created_at<=$1 AND o.published_at<=$1 AND w.processed_at<=$1 AND o.lease_until IS NULL
 AND NOT EXISTS(SELECT 1 FROM raw_events r WHERE o.kind='event' AND r.project_id=o.project_id
 AND r.environment_id=o.environment_id AND r.event_id=o.object_id)
 AND NOT EXISTS(SELECT 1 FROM work_dead_letters d WHERE d.message_id=w.message_id AND d.resolved_at IS NULL)
 ORDER BY o.published_at,o.id LIMIT $2 FOR UPDATE OF o SKIP LOCKED`, cutoff, limit)
	if err != nil {
		return result, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if len(ids) > 0 {
		tag, err := tx.Exec(ctx, `DELETE FROM processed_work WHERE message_id IN (SELECT 'switchyard-outbox-v1-'||id FROM outbox WHERE id=ANY($1::bigint[]))`, ids)
		if err != nil {
			return result, err
		}
		result.Receipts = tag.RowsAffected()
		tag, err = tx.Exec(ctx, `DELETE FROM outbox WHERE id=ANY($1::bigint[])`, ids)
		if err != nil {
			return result, err
		}
		result.Publications = tag.RowsAffected()
		if result.Receipts != result.Publications {
			return result, errors.New("work retention receipt mismatch")
		}
	}
	tag, err := tx.Exec(ctx, `WITH expired AS (SELECT stream_name,stream_sequence FROM work_dead_letters
 WHERE resolved_at<=$1 ORDER BY resolved_at,stream_name,stream_sequence LIMIT $2 FOR UPDATE SKIP LOCKED)
 DELETE FROM work_dead_letters d USING expired e WHERE d.stream_name=e.stream_name AND d.stream_sequence=e.stream_sequence`, cutoff, limit)
	if err != nil {
		return result, err
	}
	result.ResolvedFailures = tag.RowsAffected()
	if err = tx.Commit(ctx); err != nil {
		return PrunedWork{}, err
	}
	return result, nil
}
