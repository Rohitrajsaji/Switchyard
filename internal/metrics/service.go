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

type derived struct {
	Cohorts []struct {
		VariantID string `json:"variant_id"`
		Finalized bool   `json:"finalized"`
		Counts
	} `json:"cohorts"`
	Requests []struct {
		VariantID string `json:"variant_id"`
		Count     int64  `json:"count"`
		Errors    int64  `json:"errors"`
	} `json:"requests"`
	Buckets []struct {
		VariantID string `json:"variant_id"`
		Bucket
	} `json:"buckets"`
	Quality Quality `json:"quality"`
}

func (s *Service) Read(ctx context.Context, actor auth.Actor, projectID, runID string) (Results, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Results{}, err
	}
	defer tx.Rollback(context.Background())
	if err = auth.Authorize(ctx, tx, actor, projectID, "", false); err != nil {
		return Results{}, err
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
	if err = tx.QueryRow(ctx, attributionSQL+resultsSQL, projectID, definition.EnvironmentID, runID, now).Scan(&body); err != nil {
		return Results{}, err
	}
	var facts derived
	if err = json.Unmarshal(body, &facts); err != nil {
		return Results{}, err
	}
	result := assemble(definition, control, now, facts)
	addStatistics(&result)
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
