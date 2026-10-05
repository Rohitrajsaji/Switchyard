// Package events validates explicit measurement facts and commits bounded batches.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/auth"
	"switchyard/internal/outbox"
	"switchyard/pkg/evaluation"
)

const MaxBatchSize = 100
const LateAllowance = 24 * time.Hour
const FutureAllowance = 5 * time.Minute
const MaxReplayAge = 7 * 24 * time.Hour

var eventIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type Event struct {
	ID             string                     `json:"event_id"`
	Kind           string                     `json:"kind"`
	RunID          string                     `json:"run_id"`
	UserID         string                     `json:"user_id"`
	VariantID      string                     `json:"variant_id,omitempty"`
	Revision       int64                      `json:"revision"`
	DecisionID     string                     `json:"decision_id,omitempty"`
	DecisionReason string                     `json:"decision_reason"`
	ExposureID     string                     `json:"exposure_id,omitempty"`
	OccurredAt     time.Time                  `json:"occurred_at"`
	Attributes     map[string]json.RawMessage `json:"attributes,omitempty"`
	IsError        *bool                      `json:"is_error,omitempty"`
	LatencyMS      *float64                   `json:"latency_ms,omitempty"`
}
type Batch struct {
	ProjectID     string  `json:"project_id"`
	EnvironmentID string  `json:"environment_id"`
	Events        []Event `json:"events"`
}
type Receipt struct {
	EventID   string `json:"event_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	Duplicate bool   `json:"duplicate"`
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

func normalize(e Event) (Event, []byte, error) {
	if !eventIDPattern.MatchString(e.ID) || e.RunID == "" || len(e.RunID) > 128 || len(e.VariantID) > 128 || e.Revision < 1 || e.OccurredAt.IsZero() || e.OccurredAt.Year() < 2000 || e.OccurredAt.Year() > 9999 || evaluation.ValidateContext(e.UserID, e.Attributes) != nil {
		return Event{}, nil, auth.ErrInvalid
	}
	switch e.DecisionReason {
	case "experiment":
		if e.VariantID == "" {
			return Event{}, nil, auth.ErrInvalid
		}
	case "targeting", "default", "kill_switch":
	default:
		return Event{}, nil, auth.ErrInvalid
	}
	switch e.Kind {
	case "exposure":
		if e.ExposureID != "" || e.IsError != nil || e.LatencyMS != nil || !eventIDPattern.MatchString(e.DecisionID) {
			return Event{}, nil, auth.ErrInvalid
		}
	case "listing_completion":
		if !eventIDPattern.MatchString(e.ExposureID) || e.IsError != nil || e.LatencyMS != nil || e.DecisionID != "" {
			return Event{}, nil, auth.ErrInvalid
		}
	case "request_outcome":
		if !eventIDPattern.MatchString(e.ExposureID) || e.IsError == nil || e.LatencyMS == nil || e.DecisionID != "" || math.IsNaN(*e.LatencyMS) || math.IsInf(*e.LatencyMS, 0) || *e.LatencyMS < 0 || *e.LatencyMS > 60000 {
			return Event{}, nil, auth.ErrInvalid
		}
	default:
		return Event{}, nil, auth.ErrInvalid
	}
	for k, v := range e.Attributes {
		if evaluation.SensitiveAttribute(k) || (evaluation.Value{Type: "json", Data: v}).Validate("json") != nil {
			return Event{}, nil, auth.ErrInvalid
		}
	}
	// Canonical time zones and microsecond precision match PostgreSQL event-time storage.
	e.OccurredAt = e.OccurredAt.UTC().Truncate(time.Microsecond)
	body, err := json.Marshal(e)
	if err != nil || len(body) > 16384 {
		return Event{}, nil, auth.ErrInvalid
	}
	return e, body, nil
}

func quarantine(e Event, d evaluation.Definition, now time.Time) string {
	if e.OccurredAt.Before(now.Add(-LateAllowance)) {
		return "too_late"
	}
	if e.OccurredAt.After(now.Add(FutureAllowance)) {
		return "future_timestamp"
	}
	if d.Experiment == nil || d.Experiment.RunID != e.RunID {
		return "revision_run_mismatch"
	}
	c, err := evaluation.Compile(d)
	if err != nil {
		return "invalid_configuration"
	}
	result, err := c.Evaluate(e.UserID, e.Attributes)
	if err != nil {
		return "invalid_context"
	}
	if result.Reason != "experiment" {
		return "non_randomized_exposure"
	}
	if e.DecisionReason != "experiment" || result.VariantID != e.VariantID {
		return "assignment_mismatch"
	}
	return ""
}

type prepared struct {
	event    Event
	body     []byte
	position int
}

func (s *Service) Ingest(ctx context.Context, token string, in Batch) ([]Receipt, error) {
	if len(in.Events) < 1 || len(in.Events) > MaxBatchSize {
		return nil, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	app, err := auth.AuthorizeApplication(ctx, tx, token, in.ProjectID, in.EnvironmentID, "events:write")
	if err != nil {
		return nil, err
	}
	batch := make([]prepared, len(in.Events))
	for i, e := range in.Events {
		e, body, err := normalize(e)
		if err != nil {
			return nil, err
		}
		batch[i] = prepared{e, body, i}
	}
	// Overlapping concurrent batches lock unique identities in the same order.
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].event.ID < batch[j].event.ID })
	result := make([]Receipt, len(batch))
	now := s.now().UTC().Truncate(time.Microsecond)
	definitions := make(map[string]evaluation.Definition)
	for _, p := range batch {
		e := p.event
		var equal bool
		receipt := Receipt{EventID: e.ID}
		err = tx.QueryRow(ctx, receiptSQL, in.ProjectID, in.EnvironmentID, e.ID, p.body).Scan(&equal, &receipt.Status, &receipt.Reason)
		if err == nil {
			if !equal {
				return nil, auth.ErrConflict
			}
			receipt.Duplicate = true
			result[p.position] = receipt
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		// Once the original receipt is unavailable, an unsupported old replay
		// cannot create a new fact/intent after deduplication state expires.
		if e.OccurredAt.Before(now.Add(-MaxReplayAge)) {
			return nil, auth.ErrInvalid
		}
		var known bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outbox WHERE kind='event' AND project_id=$1 AND environment_id=$2 AND object_id=$3)
 OR EXISTS(SELECT 1 FROM metric_event_references WHERE project_id=$1 AND environment_id=$2 AND event_id=$3)`, in.ProjectID, in.EnvironmentID, e.ID).Scan(&known); err != nil {
			return nil, err
		}
		if known {
			return nil, auth.ErrConflict
		}
		cacheKey := e.RunID + ":" + strconv.FormatInt(e.Revision, 10)
		d, ok := definitions[cacheKey]
		reason := ""
		if !ok {
			var body []byte
			// Scope both run and historical revision; never validate against today's flag.
			err = tx.QueryRow(ctx, `SELECT f.definition FROM experiment_runs r JOIN flag_revisions f ON f.flag_id=r.flag_id AND f.environment_id=r.environment_id AND f.revision=$4 WHERE r.project_id=$1 AND r.environment_id=$2 AND r.id=$3`, in.ProjectID, in.EnvironmentID, e.RunID, e.Revision).Scan(&body)
			if errors.Is(err, pgx.ErrNoRows) {
				var exists bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM experiment_runs WHERE project_id=$1 AND environment_id=$2 AND id=$3)`, in.ProjectID, in.EnvironmentID, e.RunID).Scan(&exists); err != nil {
					return nil, err
				}
				if !exists {
					return nil, auth.ErrInvalid
				}
				reason = "unknown_revision"
			} else if err != nil {
				return nil, err
			} else {
				if err = json.Unmarshal(body, &d); err != nil {
					return nil, err
				}
				definitions[cacheKey] = d
			}
		}
		if reason == "" {
			reason = quarantine(e, d, now)
		}
		receipt.Status = "accepted"
		receipt.Reason = reason
		if reason != "" {
			receipt.Status = "quarantined"
		}
		tag, err := tx.Exec(ctx, `INSERT INTO raw_events(project_id,environment_id,event_id,run_id,user_id,kind,variant_id,revision,exposure_id,occurred_at,received_at,status,quarantine_reason,is_error,latency_ms,payload,application_key_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13,$14,$15,$16,$17) ON CONFLICT(project_id,environment_id,event_id) DO NOTHING`, in.ProjectID, in.EnvironmentID, e.ID, e.RunID, e.UserID, e.Kind, e.VariantID, e.Revision, e.ExposureID, e.OccurredAt, now, receipt.Status, receipt.Reason, e.IsError, e.LatencyMS, p.body, app.ID)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			if err = tx.QueryRow(ctx, receiptSQL, in.ProjectID, in.EnvironmentID, e.ID, p.body).Scan(&equal, &receipt.Status, &receipt.Reason); err != nil {
				return nil, err
			}
			if !equal {
				return nil, auth.ErrConflict
			}
			receipt.Duplicate = true
		}
		if !receipt.Duplicate {
			if err = outbox.Record(ctx, tx, outbox.Reference{Kind: "event", ProjectID: in.ProjectID, EnvironmentID: in.EnvironmentID, ObjectID: e.ID, Revision: e.Revision}); err != nil {
				return nil, err
			}
		}
		result[p.position] = receipt
	}
	// No acknowledgement escapes until every event fact is durable.
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
