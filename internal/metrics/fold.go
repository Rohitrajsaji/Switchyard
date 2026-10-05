package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/events"
)

const DefaultRawRetention = 7 * 24 * time.Hour
const FoldBatch = 100

type FoldOutcome struct{ Raw, ResolvedPending, PreservedPending int }

// A delivered event must have a durable receipt before its source is folded.
// The common user lock serializes folding, notification and reconciliation.
const foldableRawSQL = `SELECT e.event_id,e.received_at,true AS raw FROM raw_events e
 JOIN outbox o ON o.kind='event' AND o.project_id=e.project_id AND o.environment_id=e.environment_id
 AND o.object_id=e.event_id AND o.revision=e.revision
 JOIN processed_work w ON w.message_id='switchyard-outbox-v1-'||o.id
 WHERE e.project_id=$1 AND e.environment_id=$2 AND e.run_id=$3 AND e.user_id=$5 AND e.received_at<$6`
const resolvedPendingSQL = `SELECT p.event_id,p.received_at,false AS raw FROM metric_pending_outcomes p
 WHERE p.project_id=$1 AND p.environment_id=$2 AND p.run_id=$3 AND p.user_id=$5 AND p.received_at<$6
 AND EXISTS(SELECT 1 FROM ` + referenceSourceSQL + ` x WHERE x.project_id=p.project_id AND x.environment_id=p.environment_id
 AND x.event_id=p.exposure_id AND x.received_at<=$4)
 AND NOT EXISTS(SELECT 1 FROM raw_events e WHERE e.project_id=p.project_id AND e.environment_id=p.environment_id AND e.event_id=p.event_id)`

// FoldOne moves at most 100 facts from one user's UTC receipt day. It does not
// clear receipts, rewrite original facts, or change counters outside normal
// reconciliation. The worker does not schedule folding until final M7 gates.
func FoldOne(ctx context.Context, pool *pgxpool.Pool, now time.Time) (FoldOutcome, error) {
	return FoldWithRetention(ctx, pool, now, 7)
}

// FoldWithRetention bounds raw retention to 2..7 days: at least the late-event
// finalization interval and shorter than the eight-day identity/receipt horizon.
func FoldWithRetention(ctx context.Context, pool *pgxpool.Pool, now time.Time, days int) (FoldOutcome, error) {
	var result FoldOutcome
	if days < 2 || days > 7 || now.IsZero() {
		return result, errors.New("raw retention must be 2..7 days")
	}
	now = now.UTC().Truncate(time.Microsecond)
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	// The per-user candidate below probes every metric_user_state row. On the
	// measured database that probe took 4.6s and always exceeded the worker's
	// two-second retention budget, even though every fact was newer than the
	// cutoff. Nothing can match that candidate unless some fact is already old.
	var oldEnough bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM raw_events WHERE received_at<$1)
 OR EXISTS(SELECT 1 FROM metric_pending_outcomes WHERE received_at<$1)`, cutoff).Scan(&oldEnough)
	if err != nil {
		return result, err
	}
	if !oldEnough {
		return result, nil
	}
	var project, env, run, user string
	var historyBody []byte
	err = tx.QueryRow(ctx, `SELECT s.project_id,s.environment_id,s.run_id,s.user_id,s.historical_contribution
 FROM metric_user_state s WHERE (s.due_at IS NULL OR s.due_at>$1)
 AND NOT EXISTS(SELECT 1 FROM raw_events f WHERE f.project_id=s.project_id AND f.environment_id=s.environment_id
 AND f.run_id=s.run_id AND f.user_id=s.user_id AND (f.received_at>$1 OR (f.status='accepted' AND f.occurred_at>$1)))
 AND (EXISTS(SELECT 1 FROM raw_events e JOIN outbox o ON o.kind='event' AND o.project_id=e.project_id
 AND o.environment_id=e.environment_id AND o.object_id=e.event_id AND o.revision=e.revision
 JOIN processed_work w ON w.message_id='switchyard-outbox-v1-'||o.id
 WHERE e.project_id=s.project_id AND e.environment_id=s.environment_id AND e.run_id=s.run_id AND e.user_id=s.user_id AND e.received_at<$2)
 OR EXISTS(SELECT 1 FROM metric_pending_outcomes p JOIN `+referenceSourceSQL+` x
 ON x.project_id=p.project_id AND x.environment_id=p.environment_id AND x.event_id=p.exposure_id AND x.received_at<=$1
 WHERE p.project_id=s.project_id AND p.environment_id=s.environment_id AND p.run_id=s.run_id AND p.user_id=s.user_id AND p.received_at<$2))
 ORDER BY s.project_id,s.environment_id,s.run_id,s.user_id LIMIT 1 FOR UPDATE OF s SKIP LOCKED`, now, cutoff).Scan(&project, &env, &run, &user, &historyBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	// Archive the authoritative global earliest finalized anchor, even when a
	// late earlier exposure is in a different receipt page than its completions.
	_, err = tx.Exec(ctx, `INSERT INTO metric_archived_anchors AS a(project_id,environment_id,run_id,user_id,event_id,variant_id,occurred_at,received_at)
 SELECT $1,$2,$3,$5,event_id,variant_id,occurred_at,received_at FROM (
 SELECT event_id,variant_id,occurred_at,received_at FROM raw_events WHERE project_id=$1 AND environment_id=$2
 AND run_id=$3 AND user_id=$5 AND kind='exposure' AND status='accepted' AND received_at<=$4 AND occurred_at<=$4
 UNION ALL SELECT event_id,variant_id,occurred_at,received_at FROM metric_archived_anchors
 WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$5) sources
 WHERE occurred_at+interval '24 hours 30 minutes'<$4 ORDER BY occurred_at,event_id COLLATE "C" LIMIT 1
 ON CONFLICT(project_id,environment_id,run_id,user_id) DO UPDATE SET event_id=excluded.event_id,variant_id=excluded.variant_id,
 occurred_at=excluded.occurred_at,received_at=excluded.received_at
 WHERE (excluded.occurred_at,excluded.event_id COLLATE "C")<(a.occurred_at,a.event_id COLLATE "C")`, project, env, run, now, user)
	if err != nil {
		return result, err
	}
	args := []any{project, env, run, now, user, cutoff}
	rows, err := tx.Query(ctx, `WITH eligible AS (`+foldableRawSQL+` UNION ALL `+resolvedPendingSQL+`)
 SELECT event_id,raw,(received_at AT TIME ZONE 'UTC')::date FROM eligible
 WHERE (received_at AT TIME ZONE 'UTC')::date=(SELECT min((received_at AT TIME ZONE 'UTC')::date) FROM eligible)
 ORDER BY received_at,event_id COLLATE "C" LIMIT 100`, args...)
	if err != nil {
		return result, err
	}
	var ids, rawIDs, compactIDs []string
	var day time.Time
	for rows.Next() {
		var id string
		var raw bool
		if err = rows.Scan(&id, &raw, &day); err != nil {
			rows.Close()
			return result, err
		}
		ids = append(ids, id)
		if raw {
			rawIDs = append(rawIDs, id)
		} else {
			compactIDs = append(compactIDs, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	if len(ids) == 0 {
		return result, errors.New("fold candidate has no eligible facts")
	}
	if len(rawIDs) > 0 {
		// Lock chosen raw rows before copying/deleting them; a failed transaction
		// leaves source, identity, pending state and history all unchanged.
		if _, err = tx.Exec(ctx, `SELECT event_id FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND event_id=ANY($3::text[]) ORDER BY event_id COLLATE "C" FOR UPDATE`, project, env, rawIDs); err != nil {
			return result, err
		}
	}
	selectedSQL := strings.Replace(retainedUserSQL(), "AND user_id=$5", "AND user_id=$5 AND event_id=ANY($6::text[])", 1)
	rows, err = tx.Query(ctx, selectedSQL+`SELECT event_id FROM linked WHERE reference_status='pending'`, project, env, run, now, user, ids)
	if err != nil {
		return result, err
	}
	pending := make(map[string]bool)
	var pendingIDs []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		pending[id] = true
		pendingIDs = append(pendingIDs, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	historyIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if !pending[id] {
			historyIDs = append(historyIDs, id)
		}
	}
	if len(pendingIDs) > 0 {
		_, err = tx.Exec(ctx, `INSERT INTO metric_pending_outcomes SELECT project_id,environment_id,event_id,run_id,user_id,kind,variant_id,revision,exposure_id,occurred_at,received_at,status,quarantine_reason,is_error,latency_ms
 FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND event_id=ANY($3::text[])
 ON CONFLICT(project_id,environment_id,event_id) DO NOTHING`, project, env, pendingIDs)
		if err != nil {
			return result, err
		}
	}
	var portionBody []byte
	if err = tx.QueryRow(ctx, selectedSQL+resultsSQL, project, env, run, now, user, historyIDs).Scan(&portionBody); err != nil {
		return result, err
	}
	var history, portion derived
	if json.Unmarshal(historyBody, &history) != nil || json.Unmarshal(portionBody, &portion) != nil {
		return result, errors.New("invalid folding contribution")
	}
	// An old quarantined/pending fact cannot freeze a newly provisional cohort.
	finalized := portion.Cohorts[:0]
	for _, c := range portion.Cohorts {
		if c.Finalized {
			finalized = append(finalized, c)
		}
	}
	portion.Cohorts = finalized
	updated, err := mergeHistorical(history, portion)
	if err != nil {
		return result, err
	}
	if updated.Quality.PendingOutcomes != 0 || updated.Quality.FutureEvents != 0 {
		return result, errors.New("folding unresolved contribution")
	}
	segmentPortion := portion
	segmentPortion.Cohorts = nil
	segmentPortion.Quality.DuplicateAttributedCompletions = updated.Quality.DuplicateAttributedCompletions - history.Quality.DuplicateAttributedCompletions
	var segmentBody []byte
	err = tx.QueryRow(ctx, `SELECT contribution FROM metric_history_segments WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4 AND receipt_day=$5`, project, env, run, user, day).Scan(&segmentBody)
	var segment derived
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if err == nil && json.Unmarshal(segmentBody, &segment) != nil {
		return result, errors.New("invalid historical segment")
	}
	segment, err = mergeHistorical(segment, segmentPortion)
	if err != nil {
		return result, err
	}
	historyBody, err = json.Marshal(updated)
	if err != nil {
		return result, err
	}
	segmentBody, err = json.Marshal(segment)
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO metric_history_segments SELECT $1,$2,$3,$4,$5::date,$6::jsonb
 WHERE $5::date>=(SELECT (reporting_since AT TIME ZONE 'UTC')::date FROM metric_user_state
 WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4)
 ON CONFLICT(project_id,environment_id,run_id,user_id,receipt_day) DO UPDATE SET contribution=excluded.contribution`, project, env, run, user, day, segmentBody); err != nil {
		return result, err
	}
	converted := false
	for _, c := range updated.Cohorts {
		converted = converted || c.Converted > 0
	}
	if _, err = tx.Exec(ctx, `UPDATE metric_archived_anchors SET converted=converted OR $5 WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4`, project, env, run, user, converted); err != nil {
		return result, err
	}
	if len(rawIDs) > 0 {
		if _, err = events.PreserveIdentities(ctx, tx, project, env, rawIDs); err != nil {
			return result, err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO metric_event_references SELECT project_id,environment_id,event_id,run_id,user_id,kind,status,variant_id,occurred_at,received_at FROM raw_events
 WHERE project_id=$1 AND environment_id=$2 AND event_id=ANY($3::text[]) ON CONFLICT(project_id,environment_id,event_id) DO NOTHING`, project, env, rawIDs); err != nil {
			return result, err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND event_id=ANY($3::text[])`, project, env, rawIDs); err != nil {
			return result, err
		}
	}
	if len(compactIDs) > 0 {
		if _, err = tx.Exec(ctx, `DELETE FROM metric_pending_outcomes WHERE project_id=$1 AND environment_id=$2 AND event_id=ANY($3::text[])`, project, env, compactIDs); err != nil {
			return result, err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE metric_user_state SET historical_contribution=$5,due_at=$6 WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4`, project, env, run, user, historyBody, now); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	return FoldOutcome{len(rawIDs), len(compactIDs), len(pendingIDs)}, nil
}
