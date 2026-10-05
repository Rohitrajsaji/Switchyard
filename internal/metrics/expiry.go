package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const DefaultSummaryDays = 90

type ExpiredSummary struct {
	User              bool
	Segments, Pending int64
}

// SummaryFloor keeps the current UTC receipt day and the preceding days-1 days.
// Calendar days make a compact daily segment the exact expiry unit.
func SummaryFloor(now time.Time, days int) (time.Time, error) {
	if now.IsZero() || days < 8 || days > 365 {
		return time.Time{}, errors.New("summary retention must be 8..365 UTC days")
	}
	return now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1)), nil
}

// ExpireOne expires one user's reporting data under the shared user lock.
// Each deletion group is bounded at 100; rebuilding uses at most days daily
// summaries and one original anchor. Counters change in normal reconciliation.
// Anchor/reference identities are preserved to prevent attribution resurrection.
func ExpireOne(ctx context.Context, pool *pgxpool.Pool, now time.Time, days int) (ExpiredSummary, error) {
	var result ExpiredSummary
	floor, err := SummaryFloor(now, days)
	if err != nil {
		return result, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(context.Background())
	var project, env, run, user string
	err = tx.QueryRow(ctx, `SELECT s.project_id,s.environment_id,s.run_id,s.user_id FROM metric_user_state s
 WHERE EXISTS(SELECT 1 FROM metric_history_segments h WHERE h.project_id=s.project_id AND h.environment_id=s.environment_id AND h.run_id=s.run_id AND h.user_id=s.user_id AND h.receipt_day<($1 AT TIME ZONE 'UTC')::date)
 OR EXISTS(SELECT 1 FROM metric_pending_outcomes p WHERE p.project_id=s.project_id AND p.environment_id=s.environment_id AND p.run_id=s.run_id AND p.user_id=s.user_id AND p.received_at<$1)
 OR (s.reporting_since<$1 AND (EXISTS(SELECT 1 FROM raw_events e WHERE e.project_id=s.project_id AND e.environment_id=s.environment_id AND e.run_id=s.run_id AND e.user_id=s.user_id AND e.received_at<$1)
 OR EXISTS(SELECT 1 FROM metric_archived_anchors a WHERE a.project_id=s.project_id AND a.environment_id=s.environment_id AND a.run_id=s.run_id AND a.user_id=s.user_id AND a.received_at<$1
 AND jsonb_array_length(COALESCE(s.historical_contribution->'cohorts','[]'::jsonb))>0)))
 ORDER BY s.project_id,s.environment_id,s.run_id,s.user_id LIMIT 1 FOR UPDATE OF s SKIP LOCKED`, floor).Scan(&project, &env, &run, &user)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var currentFloor time.Time
	if err = tx.QueryRow(ctx, `UPDATE metric_user_state SET reporting_since=GREATEST(reporting_since,$5) WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4 RETURNING reporting_since`, project, env, run, user, floor).Scan(&currentFloor); err != nil {
		return result, err
	}
	// Never recreate data after a clock regression or retention-window increase.
	floor = currentFloor
	tag, err := tx.Exec(ctx, `WITH selected AS (SELECT receipt_day FROM metric_history_segments WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4
 AND receipt_day<($5 AT TIME ZONE 'UTC')::date ORDER BY receipt_day LIMIT 100 FOR UPDATE)
 DELETE FROM metric_history_segments h USING selected x WHERE h.project_id=$1 AND h.environment_id=$2 AND h.run_id=$3 AND h.user_id=$4 AND h.receipt_day=x.receipt_day`, project, env, run, user, floor)
	if err != nil {
		return result, err
	}
	result.Segments = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `WITH selected AS (SELECT event_id FROM metric_pending_outcomes WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4
 AND received_at<$5 ORDER BY received_at,event_id COLLATE "C" LIMIT 100 FOR UPDATE)
 DELETE FROM metric_pending_outcomes p USING selected x WHERE p.project_id=$1 AND p.environment_id=$2 AND p.event_id=x.event_id`, project, env, run, user, floor)
	if err != nil {
		return result, err
	}
	result.Pending = tag.RowsAffected()
	rows, err := tx.Query(ctx, `SELECT contribution FROM metric_history_segments WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4
 AND receipt_day>=($5 AT TIME ZONE 'UTC')::date ORDER BY receipt_day LIMIT 366`, project, env, run, user, floor)
	if err != nil {
		return result, err
	}
	history := derived{}
	count := 0
	for rows.Next() {
		var body []byte
		if err = rows.Scan(&body); err != nil {
			break
		}
		var segment derived
		if err = json.Unmarshal(body, &segment); err != nil {
			break
		}
		if len(segment.Cohorts) != 0 {
			err = errors.New("daily summary contains cohort identity")
			break
		}
		history, err = mergeHistorical(history, segment)
		if err != nil {
			break
		}
		count++
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return result, err
	}
	if count > 365 {
		return result, errors.New("historical summaries exceed bounded calendar window")
	}
	var variant string
	var converted bool
	err = tx.QueryRow(ctx, `SELECT variant_id,converted FROM metric_archived_anchors WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4 AND received_at>=$5`, project, env, run, user, floor).Scan(&variant, &converted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	if err == nil {
		var n int64
		if converted {
			n = 1
		}
		history.Cohorts = []cohortCounts{{VariantID: variant, Finalized: true, Counts: Counts{1, n}}}
	}
	body, err := json.Marshal(history)
	if err != nil {
		return result, err
	}
	if _, err = tx.Exec(ctx, `UPDATE metric_user_state SET historical_contribution=$5,due_at=$6 WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4`, project, env, run, user, body, now); err != nil {
		return result, err
	}
	if err = tx.Commit(ctx); err != nil {
		return ExpiredSummary{}, err
	}
	result.User = true
	return result, nil
}
