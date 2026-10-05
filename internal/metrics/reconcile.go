package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnqueueUser commits work in the caller's transaction. Taking the same row
// lock as reconciliation prevents a notification from being lost at completion.
func EnqueueUser(ctx context.Context, tx pgx.Tx, project, env, run, user string) error {
	_, err := tx.Exec(ctx, `INSERT INTO metric_user_state(project_id,environment_id,run_id,user_id,due_at)
        VALUES($1,$2,$3,$4,clock_timestamp()) ON CONFLICT(project_id,environment_id,run_id,user_id)
        DO UPDATE SET due_at=LEAST(metric_user_state.due_at,excluded.due_at)`, project, env, run, user)
	return err
}

// Fact arrival also resolves cross-user/run invalid references that were
// previously classified as pending. Schedule all affected users, not just the
// user named by the exposure. The source fact remains authoritative.
func EnqueueReferencingUsers(ctx context.Context, tx pgx.Tx, project, env, event string) error {
	_, err := tx.Exec(ctx, `INSERT INTO metric_user_state(project_id,environment_id,run_id,user_id,due_at)
		SELECT project_id,environment_id,run_id,user_id,clock_timestamp()
		FROM (SELECT DISTINCT project_id,environment_id,run_id,user_id
		FROM (SELECT project_id,environment_id,run_id,user_id FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND exposure_id=$3
 UNION ALL SELECT project_id,environment_id,run_id,user_id FROM metric_pending_outcomes WHERE project_id=$1 AND environment_id=$2 AND exposure_id=$3) sources) affected
        ORDER BY project_id,environment_id,run_id,user_id
        ON CONFLICT(project_id,environment_id,run_id,user_id)
        DO UPDATE SET due_at=LEAST(metric_user_state.due_at,excluded.due_at)`, project, env, event)
	return err
}

type dimension struct {
	variant, category, metric string
	bucket                    int
}

func contributionCounts(d derived) map[dimension]int64 {
	m := make(map[dimension]int64)
	for _, c := range d.Cohorts {
		category := "provisional"
		if c.Finalized {
			category = "finalized"
		}
		m[dimension{c.VariantID, category, "exposed", 0}] += c.Exposed
		m[dimension{c.VariantID, category, "converted", 0}] += c.Converted
	}
	for _, r := range d.Requests {
		m[dimension{r.VariantID, "request", "count", 0}] += r.Count
		m[dimension{r.VariantID, "request", "errors", 0}] += r.Errors
	}
	for _, b := range d.Buckets {
		m[dimension{b.VariantID, "latency", "count", b.UpperBoundMS}] += b.Count
	}
	for name, n := range map[string]int64{
		"quarantined_events": d.Quality.QuarantinedEvents, "future_events": d.Quality.FutureEvents,
		"pending_outcomes": d.Quality.PendingOutcomes, "invalid_reference_outcomes": d.Quality.InvalidReferenceOutcomes,
		"outside_window_completions": d.Quality.OutsideWindowCompletions, "duplicate_attributed_completions": d.Quality.DuplicateAttributedCompletions,
	} {
		m[dimension{"", "quality", name, 0}] = n
	}
	return m
}

// ReconcileOne serializes only one user contribution per transaction. Counters,
// contribution and next deadline commit together, making retries idempotent.
// Workers keep a fixed loop; this function creates no per-user goroutine.
func ReconcileOne(ctx context.Context, pool *pgxpool.Pool, now time.Time) (bool, error) {
	now = now.UTC().Truncate(time.Microsecond)
	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.Background())
	var project, env, run, user string
	var oldBody, historyBody []byte
	err = tx.QueryRow(ctx, `SELECT project_id,environment_id,run_id,user_id,contribution,historical_contribution FROM metric_user_state
        WHERE due_at<=$1 ORDER BY due_at,project_id,environment_id,run_id,user_id LIMIT 1 FOR UPDATE SKIP LOCKED`, now).Scan(&project, &env, &run, &user, &oldBody, &historyBody)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Reuse the MVP semantics with a single-user fact set. References still join
	// the full scoped raw table, so mismatched identities remain invalid.
	userSQL := retainedUserSQL()
	var newBody []byte
	var next *time.Time
	err = tx.QueryRow(ctx, userSQL+`SELECT (`+resultsSQL+`), LEAST(
		(SELECT min(received_at) FROM `+measurementSourceSQL+` source WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$5 AND received_at>$4),
		(SELECT min(x.received_at) FROM facts e JOIN `+referenceSourceSQL+` x ON x.project_id=e.project_id AND x.environment_id=e.environment_id
		 AND x.event_id=e.exposure_id WHERE e.status='accepted' AND x.received_at>$4),
		(SELECT min(occurred_at) FROM facts WHERE status='accepted' AND occurred_at>$4),
        (SELECT min(occurred_at+interval '24 hours 30 minutes'+interval '1 microsecond') FROM anchors
         WHERE occurred_at+interval '24 hours 30 minutes'+interval '1 microsecond'>$4))`, project, env, run, now, user).Scan(&newBody, &next)
	if err != nil {
		return false, err
	}
	var old, new, history derived
	if json.Unmarshal(oldBody, &old) != nil || json.Unmarshal(newBody, &new) != nil || json.Unmarshal(historyBody, &history) != nil {
		return false, errors.New("invalid stored metric contribution")
	}
	new, err = mergeHistorical(history, new)
	if err != nil {
		return false, err
	}
	newBody, err = json.Marshal(new)
	if err != nil {
		return false, err
	}
	deltas := contributionCounts(new)
	for k, n := range contributionCounts(old) {
		deltas[k] -= n
	}
	keys := make([]dimension, 0, len(deltas))
	for k, n := range deltas {
		if n != 0 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.variant != b.variant {
			return a.variant < b.variant
		}
		if a.category != b.category {
			return a.category < b.category
		}
		if a.metric != b.metric {
			return a.metric < b.metric
		}
		return a.bucket < b.bucket
	})
	for _, k := range keys {
		delta := deltas[k]
		if delta < 0 {
			tag, err := tx.Exec(ctx, `UPDATE metric_counts SET value=value+$8 WHERE project_id=$1 AND environment_id=$2 AND run_id=$3
                AND variant_id=$4 AND category=$5 AND metric=$6 AND bucket=$7`, project, env, run, k.variant, k.category, k.metric, k.bucket, delta)
			if err != nil {
				return false, err
			}
			if tag.RowsAffected() != 1 {
				return false, errors.New("metric subtraction missing its prior counter")
			}
		} else {
			_, err := tx.Exec(ctx, `INSERT INTO metric_counts(project_id,environment_id,run_id,variant_id,category,metric,bucket,value)
                VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(project_id,environment_id,run_id,variant_id,category,metric,bucket)
                DO UPDATE SET value=metric_counts.value+excluded.value`, project, env, run, k.variant, k.category, k.metric, k.bucket, delta)
			if err != nil {
				return false, err
			}
		}
	}
	_, err = tx.Exec(ctx, `UPDATE metric_user_state SET contribution=$5,reconciled_at=$6,due_at=$7
        WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND user_id=$4`, project, env, run, user, newBody, now, next)
	if err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}
