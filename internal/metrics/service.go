// Package metrics derives experiment results from committed raw event facts.
package metrics

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/auth"
	"switchyard/internal/experiments"
	"switchyard/pkg/evaluation"
)

//go:embed attribution.sql
var attributionSQL string

//go:embed results.sql
var resultsSQL string

//go:embed materialized.sql
var materializedSQL string

type Counts struct {
	Exposed   int64 `json:"exposed"`
	Converted int64 `json:"converted"`
}
type Bucket struct {
	UpperBoundMS int   `json:"upper_bound_ms"`
	Count        int64 `json:"count"`
}
type Requests struct {
	Count           int64    `json:"count"`
	Errors          int64    `json:"errors"`
	ErrorRate       *float64 `json:"error_rate"`
	P95UpperBoundMS *int     `json:"p95_upper_bound_ms"`
	Histogram       []Bucket `json:"histogram"`
}
type Variant struct {
	ID              string     `json:"id"`
	WeightBP        int        `json:"weight_bp"`
	Provisional     Counts     `json:"provisional"`
	Finalized       Counts     `json:"finalized"`
	Total           Counts     `json:"total"`
	Requests        Requests   `json:"requests"`
	ProvisionalRate Proportion `json:"provisional_rate"`
	FinalizedRate   Proportion `json:"finalized_rate"`
	TotalRate       Proportion `json:"total_rate"`
}
type Quality struct {
	QuarantinedEvents              int64 `json:"quarantined_events"`
	FutureEvents                   int64 `json:"future_events"`
	PendingOutcomes                int64 `json:"pending_outcomes"`
	InvalidReferenceOutcomes       int64 `json:"invalid_reference_outcomes"`
	OutsideWindowCompletions       int64 `json:"outside_window_completions"`
	DuplicateAttributedCompletions int64 `json:"duplicate_attributed_completions"`
}
type Results struct {
	RunID                    string       `json:"run_id"`
	EnvironmentID            string       `json:"environment_id"`
	ControlVariantID         string       `json:"control_variant_id"`
	AsOf                     time.Time    `json:"as_of"`
	AttributionWindowSeconds int          `json:"attribution_window_seconds"`
	LateAllowanceSeconds     int          `json:"late_allowance_seconds"`
	Variants                 []Variant    `json:"variants"`
	Quality                  Quality      `json:"quality"`
	SampleRatio              SampleRatios `json:"sample_ratio"`
	Comparisons              []Comparison `json:"comparisons"`
	InferenceNotes           []string     `json:"inference_notes"`
	Processing               *Processing  `json:"processing,omitempty"`
}

// Processing describes unfinished work at the same database snapshot as counts.
// LatestReconciledAt is a latest update, never a completeness watermark.
type Processing struct {
	PendingEvents      int64      `json:"pending_events"`
	DueUsers           int64      `json:"due_users"`
	OldestPendingAt    *time.Time `json:"oldest_pending_at"`
	LatestReconciledAt *time.Time `json:"latest_reconciled_at"`
	LagSeconds         float64    `json:"lag_seconds"`
}
type Service struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func New(pool *pgxpool.Pool, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, now: now}
}

type cohortCounts struct {
	VariantID string `json:"variant_id"`
	Finalized bool   `json:"finalized"`
	Counts
}
type requestCounts struct {
	VariantID string `json:"variant_id"`
	Count     int64  `json:"count"`
	Errors    int64  `json:"errors"`
}
type bucketCounts struct {
	VariantID string `json:"variant_id"`
	Bucket
}
type derived struct {
	Cohorts  []cohortCounts  `json:"cohorts"`
	Requests []requestCounts `json:"requests"`
	Buckets  []bucketCounts  `json:"buckets"`
	Quality  Quality         `json:"quality"`
}

func (s *Service) Read(ctx context.Context, actor auth.Actor, projectID, runID string) (Results, error) {
	return s.read(ctx, actor, projectID, runID, false, false, true)
}

// ReadAggregated supplies the counts-only view for independent parity checks.
func (s *Service) ReadAggregated(ctx context.Context, actor auth.Actor, projectID, runID string) (Results, error) {
	return s.read(ctx, actor, projectID, runID, true, false, true)
}

// ReadAsync serves durable aggregates with visible backlog. Raw facts are never
// an automatic fallback: they cannot reconstruct already-retained history.
func (s *Service) ReadAsync(ctx context.Context, actor auth.Actor, projectID, runID string) (Results, error) {
	return s.read(ctx, actor, projectID, runID, true, true, true)
}

// ReadForPolicy serves durable aggregates to trusted in-process policy code (rollout
// guardrails) that has no human actor. It is not reachable from any HTTP handler.
func (s *Service) ReadForPolicy(ctx context.Context, projectID, runID string) (Results, error) {
	return s.read(ctx, auth.Actor{}, projectID, runID, true, true, false)
}

func (s *Service) read(ctx context.Context, actor auth.Actor, projectID, runID string, aggregated, progress, authorize bool) (Results, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Results{}, err
	}
	defer tx.Rollback(context.Background())
	if authorize {
		if err = auth.Authorize(ctx, tx, actor, projectID, "", false); err != nil {
			return Results{}, err
		}
	}
	var body []byte
	var control string
	err = tx.QueryRow(ctx, `SELECT definition,control_variant_id FROM experiment_runs WHERE project_id=$1 AND id=$2`, projectID, runID).Scan(&body, &control)
	if errors.Is(err, pgx.ErrNoRows) {
		return Results{}, experiments.ErrNotFound
	}
	if err != nil {
		return Results{}, err
	}
	var definition evaluation.Definition
	if err = json.Unmarshal(body, &definition); err != nil {
		return Results{}, err
	}
	if _, err = evaluation.Compile(definition); err != nil {
		return Results{}, err
	}
	if definition.Experiment == nil {
		return Results{}, errors.New("run definition missing assignment")
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if aggregated {
		err = tx.QueryRow(ctx, materializedSQL, projectID, definition.EnvironmentID, runID).Scan(&body)
	} else {
		err = tx.QueryRow(ctx, attributionSQL+resultsSQL, projectID, definition.EnvironmentID, runID, now).Scan(&body)
	}
	if err != nil {
		return Results{}, err
	}
	var facts derived
	if err = json.Unmarshal(body, &facts); err != nil {
		return Results{}, err
	}
	result := assemble(definition, control, now, facts)
	addStatistics(&result)
	if progress {
		status := &Processing{}
		err = tx.QueryRow(ctx, `WITH pending AS (
 SELECT e.received_at FROM raw_events e
 LEFT JOIN outbox o ON o.kind='event' AND o.project_id=e.project_id AND o.environment_id=e.environment_id AND o.object_id=e.event_id AND o.revision=e.revision
 LEFT JOIN processed_work p ON p.message_id='switchyard-outbox-v1-'||o.id::text
 WHERE e.project_id=$1 AND e.environment_id=$2 AND e.run_id=$3 AND p.message_id IS NULL
), users AS (
 SELECT due_at,reconciled_at FROM metric_user_state WHERE project_id=$1 AND environment_id=$2 AND run_id=$3
)
SELECT (SELECT count(*) FROM pending),
 (SELECT count(*) FROM users WHERE due_at<=$4),
 LEAST((SELECT min(received_at) FROM pending),(SELECT min(due_at) FROM users WHERE due_at<=$4)),
 (SELECT max(reconciled_at) FROM users)`, projectID, definition.EnvironmentID, runID, now).
			Scan(&status.PendingEvents, &status.DueUsers, &status.OldestPendingAt, &status.LatestReconciledAt)
		if err != nil {
			return Results{}, err
		}
		if status.OldestPendingAt != nil {
			status.LagSeconds = max(0, now.Sub(*status.OldestPendingAt).Seconds())
		}
		result.Processing = status
		result.InferenceNotes = append(result.InferenceNotes, "Counts are asynchronous worker aggregates. Snapshot time is the read time, not a completeness watermark; pending work can change these results. Raw facts are not an automatic fallback after retention.")
	}
	if err = tx.Commit(ctx); err != nil {
		return Results{}, err
	}
	return result, nil
}

var histogramBounds = []int{50, 100, 250, 500, 1000, 2500, 5000, 10000, 60000}

func assemble(d evaluation.Definition, control string, now time.Time, facts derived) Results {
	result := Results{RunID: d.Experiment.RunID, EnvironmentID: d.EnvironmentID, ControlVariantID: control, AsOf: now, AttributionWindowSeconds: 1800, LateAllowanceSeconds: 86400, Quality: facts.Quality, Variants: make([]Variant, 0, len(d.Experiment.Variants))}
	variants := append([]evaluation.Variant(nil), d.Experiment.Variants...)
	sort.Slice(variants, func(i, j int) bool { return variants[i].Ordinal < variants[j].Ordinal })
	for _, v := range variants {
		item := Variant{ID: v.ID, WeightBP: v.WeightBP, Requests: Requests{Histogram: make([]Bucket, len(histogramBounds))}}
		for _, c := range facts.Cohorts {
			if c.VariantID == v.ID {
				if c.Finalized {
					item.Finalized = c.Counts
				} else {
					item.Provisional = c.Counts
				}
				item.Total.Exposed += c.Exposed
				item.Total.Converted += c.Converted
			}
		}
		for _, r := range facts.Requests {
			if r.VariantID == v.ID {
				item.Requests.Count = r.Count
				item.Requests.Errors = r.Errors
			}
		}
		for i, bound := range histogramBounds {
			item.Requests.Histogram[i].UpperBoundMS = bound
			for _, b := range facts.Buckets {
				if b.VariantID == v.ID && b.UpperBoundMS == bound {
					item.Requests.Histogram[i].Count = b.Count
				}
			}
		}
		if item.Requests.Count > 0 {
			rate := float64(item.Requests.Errors) / float64(item.Requests.Count)
			item.Requests.ErrorRate = &rate
			// Integer nearest-rank p95, reported as its enclosing histogram upper bound.
			threshold := item.Requests.Count - item.Requests.Count/20
			var cumulative int64
			for _, b := range item.Requests.Histogram {
				cumulative += b.Count
				if cumulative >= threshold {
					bound := b.UpperBoundMS
					item.Requests.P95UpperBoundMS = &bound
					break
				}
			}
		}
		result.Variants = append(result.Variants, item)
	}
	return result
}
