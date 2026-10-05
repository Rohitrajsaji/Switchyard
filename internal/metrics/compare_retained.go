package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Historical daily segments/anchor identity are the retained source of truth.
// Validate their cached user summary separately from materialized counters.
func historicalExpected(ctx context.Context, tx pgx.Tx, project, env, run, user string) (derived, error) {
	history := derived{}
	rows, err := tx.Query(ctx, `SELECT h.contribution FROM metric_history_segments h JOIN metric_user_state s USING(project_id,environment_id,run_id,user_id)
 WHERE h.project_id=$1 AND h.environment_id=$2 AND h.run_id=$3 AND h.user_id=$4
 AND h.receipt_day>=(s.reporting_since AT TIME ZONE 'UTC')::date ORDER BY h.receipt_day LIMIT 366`, project, env, run, user)
	if err != nil {
		return history, err
	}
	n := 0
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
			err = errors.New("historical day contains a cohort")
			break
		}
		history, err = mergeHistorical(history, segment)
		if err != nil {
			break
		}
		n++
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return derived{}, err
	}
	if n > 365 {
		return derived{}, errors.New("historical parity exceeds retained daily bound")
	}
	var variant string
	var converted bool
	err = tx.QueryRow(ctx, `SELECT a.variant_id,a.converted FROM metric_archived_anchors a JOIN metric_user_state s USING(project_id,environment_id,run_id,user_id)
 WHERE a.project_id=$1 AND a.environment_id=$2 AND a.run_id=$3 AND a.user_id=$4 AND a.received_at>=s.reporting_since`, project, env, run, user).Scan(&variant, &converted)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return derived{}, err
	}
	if err == nil {
		var c int64
		if converted {
			c = 1
		}
		history.Cohorts = []cohortCounts{{VariantID: variant, Finalized: true, Counts: Counts{1, c}}}
	}
	return history, nil
}

func compareRetained(ctx context.Context, tx pgx.Tx, project, env, run string, now time.Time) (bool, error) {
	expected := map[dimension]int64{}
	cursor := ""
	for {
		rows, err := tx.Query(ctx, `SELECT user_id FROM (SELECT user_id FROM metric_user_state WHERE project_id=$1 AND environment_id=$2 AND run_id=$3
 UNION SELECT user_id FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND run_id=$3
 UNION SELECT user_id FROM metric_pending_outcomes WHERE project_id=$1 AND environment_id=$2 AND run_id=$3) users
 WHERE user_id COLLATE "C">$4 COLLATE "C" ORDER BY user_id COLLATE "C" LIMIT 100`, project, env, run, cursor)
		if err != nil {
			return false, err
		}
		var users []string
		for rows.Next() {
			var user string
			if err = rows.Scan(&user); err != nil {
				break
			}
			users = append(users, user)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return false, err
		}
		for _, user := range users {
			var cachedBody, retainedBody []byte
			err = tx.QueryRow(ctx, `SELECT historical_contribution FROM metric_user_state WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4`, project, env, run, user).Scan(&cachedBody)
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			var cached derived
			if err = json.Unmarshal(cachedBody, &cached); err != nil {
				return false, err
			}
			history, err := historicalExpected(ctx, tx, project, env, run, user)
			if err != nil {
				return false, err
			}
			if !equalCounts(contributionCounts(cached), contributionCounts(history)) {
				return false, nil
			}
			if err = tx.QueryRow(ctx, retainedUserSQL()+resultsSQL, project, env, run, now, user).Scan(&retainedBody); err != nil {
				return false, err
			}
			var current derived
			if err = json.Unmarshal(retainedBody, &current); err != nil {
				return false, err
			}
			combined, err := mergeHistorical(history, current)
			if err != nil {
				return false, err
			}
			for k, n := range contributionCounts(combined) {
				expected[k] += n
			}
			cursor = user
		}
		if len(users) < 100 {
			break
		}
	}
	var body []byte
	if err := tx.QueryRow(ctx, materializedSQL, project, env, run).Scan(&body); err != nil {
		return false, err
	}
	var actual derived
	if err := json.Unmarshal(body, &actual); err != nil {
		return false, err
	}
	return equalCounts(expected, contributionCounts(actual)), nil
}

func equalCounts(a, b map[dimension]int64) bool {
	for k, n := range a {
		if b[k] != n {
			return false
		}
	}
	for k, n := range b {
		if a[k] != n {
			return false
		}
	}
	return true
}
