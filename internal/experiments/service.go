// Package experiments owns immutable randomized populations and their lifecycle.
package experiments

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/audit"
	"switchyard/internal/auth"
	"switchyard/internal/flags"
	"switchyard/internal/platform/identity"
	"switchyard/pkg/evaluation"
)

var ErrNotFound = errors.New("experiment not found")

type Run struct {
	ID                    string                `json:"id"`
	Name                  string                `json:"name"`
	State                 string                `json:"state"`
	ControlVariantID      string                `json:"control_variant_id"`
	Definition            evaluation.Definition `json:"definition"`
	CreatedAt             time.Time             `json:"created_at"`
	StartedAt             *time.Time            `json:"started_at,omitempty"`
	CompletedAt           *time.Time            `json:"completed_at,omitempty"`
	ConfigurationRevision int64                 `json:"configuration_revision"`
}
type CreateInput struct {
	EnvironmentID    string               `json:"environment_id"`
	FlagKey          string               `json:"flag_key"`
	ExpectedRevision int64                `json:"expected_revision"`
	Name             string               `json:"name"`
	ControlVariantID string               `json:"control_variant_id"`
	TrafficBP        int                  `json:"traffic_bp"`
	Variants         []evaluation.Variant `json:"variants"`
	Reason           string               `json:"reason"`
}
type TransitionInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Action           string `json:"action"`
	Reason           string `json:"reason"`
}
type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Create(ctx context.Context, actor auth.Actor, projectID string, in CreateInput, requestID string) (Run, error) {
	if len(in.Name) < 1 || len(in.Name) > 128 || len(in.Reason) < 1 || len(in.Reason) > 512 || in.ExpectedRevision < 1 {
		return Run{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback(context.Background())
	if err = auth.Authorize(ctx, tx, actor, projectID, in.EnvironmentID, true); err != nil {
		return Run{}, err
	}
	d, err := flags.LockCurrent(ctx, tx, projectID, in.EnvironmentID, in.FlagKey)
	if err != nil {
		return Run{}, err
	}
	if d.Revision != in.ExpectedRevision || d.Experiment != nil || d.Rollout != nil || d.Killed {
		return Run{}, auth.ErrConflict
	}
	var reserved bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM experiment_runs WHERE flag_id=$1 AND environment_id=$2 AND state<>'completed')`, d.FlagID, in.EnvironmentID).Scan(&reserved); err != nil {
		return Run{}, err
	}
	if reserved {
		return Run{}, auth.ErrConflict
	}
	found := false
	for _, v := range in.Variants {
		if v.ID == in.ControlVariantID {
			found = true
		}
	}
	if !found {
		return Run{}, auth.ErrInvalid
	}
	id := identity.New("run_")
	d.Experiment = &evaluation.Experiment{RunID: id, EligibilitySalt: identity.New("elig_"), VariantSalt: identity.New("var_"), TrafficBP: in.TrafficBP, Variants: in.Variants}
	compiled, err := evaluation.Compile(d)
	if err != nil {
		return Run{}, auth.ErrInvalid
	}
	body, err := json.Marshal(compiled)
	if err != nil {
		return Run{}, err
	}
	// Normalize through jsonb and validate before freezing the persisted population.
	if err = tx.QueryRow(ctx, `SELECT $1::jsonb`, body).Scan(&body); err != nil {
		return Run{}, auth.ErrInvalid
	}
	if err = json.Unmarshal(body, &d); err != nil {
		return Run{}, err
	}
	if _, err = evaluation.Compile(d); err != nil {
		return Run{}, auth.ErrInvalid
	}
	var created time.Time
	err = tx.QueryRow(ctx, `INSERT INTO experiment_runs(id,project_id,environment_id,flag_id,name,state,control_variant_id,definition,created_by) VALUES($1,$2,$3,$4,$5,'draft',$6,$7,$8) RETURNING created_at`, id, projectID, in.EnvironmentID, d.FlagID, in.Name, in.ControlVariantID, body, actor.ID).Scan(&created)
	if err != nil {
		return Run{}, err
	}
	details, _ := json.Marshal(map[string]string{"run_id": id, "flag_key": d.Key})
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: "human", ProjectID: projectID, EnvironmentID: in.EnvironmentID, Action: "experiment.created", RequestID: requestID, Reason: in.Reason, Details: details}); err != nil {
		return Run{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Run{}, err
	}
	return Run{ID: id, Name: in.Name, State: "draft", ControlVariantID: in.ControlVariantID, Definition: d, CreatedAt: created, ConfigurationRevision: d.Revision}, nil
}

const runColumns = `r.id,r.name,r.state,r.control_variant_id,r.definition,r.created_at,r.started_at,r.completed_at,s.current_revision`
const runTables = `experiment_runs r JOIN environment_flag_state s ON s.flag_id=r.flag_id AND s.environment_id=r.environment_id`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var body []byte
	err := row.Scan(&r.ID, &r.Name, &r.State, &r.ControlVariantID, &body, &r.CreatedAt, &r.StartedAt, &r.CompletedAt, &r.ConfigurationRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	err = json.Unmarshal(body, &r.Definition)
	return r, err
}

func readRun(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, projectID, id string) (Run, error) {
	return scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM `+runTables+` WHERE r.project_id=$1 AND r.id=$2`, projectID, id))
}

func (s *Service) List(ctx context.Context, actor auth.Actor, projectID, environmentID string) ([]Run, error) {
	if environmentID == "" {
		return nil, auth.ErrInvalid
	}
	if err := auth.Authorize(ctx, s.pool, actor, projectID, environmentID, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+runColumns+` FROM `+runTables+` WHERE r.project_id=$1 AND r.environment_id=$2 ORDER BY r.created_at DESC,r.id DESC LIMIT 100`, projectID, environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Run, 0)
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (s *Service) Get(ctx context.Context, actor auth.Actor, projectID, id string) (Run, error) {
	// Authorize project membership before looking up a scoped run.
	if err := auth.Authorize(ctx, s.pool, actor, projectID, "", false); err != nil {
		return Run{}, err
	}
	return readRun(ctx, s.pool, projectID, id)
}

func nextState(state, action string) (string, error) {
	switch action {
	case "start":
		if state == "draft" || state == "paused" {
			return "running", nil
		}
	case "pause":
		if state == "running" {
			return "paused", nil
		}
	case "complete":
		if state == "draft" || state == "running" || state == "paused" {
			return "completed", nil
		}
	default:
		return "", auth.ErrInvalid
	}
	return "", auth.ErrConflict
}

func (s *Service) Transition(ctx context.Context, actor auth.Actor, projectID, id string, in TransitionInput, requestID string) (Run, error) {
	if len(in.Reason) < 1 || len(in.Reason) > 512 || in.ExpectedRevision < 1 {
		return Run{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback(context.Background())
	if err = auth.Authorize(ctx, tx, actor, projectID, "", false); err != nil {
		return Run{}, err
	}
	r, err := readRun(ctx, tx, projectID, id)
	if err != nil {
		return Run{}, err
	}
	if err = auth.Authorize(ctx, tx, actor, projectID, r.Definition.EnvironmentID, true); err != nil {
		return Run{}, err
	}
	d, err := flags.LockCurrent(ctx, tx, projectID, r.Definition.EnvironmentID, r.Definition.Key)
	if err != nil {
		return Run{}, err
	}
	// Re-read after acquiring the shared flag lock: another transition may have won.
	r, err = readRun(ctx, tx, projectID, id)
	if err != nil {
		return Run{}, err
	}
	state, err := nextState(r.State, in.Action)
	if err != nil {
		return Run{}, err
	}
	if d.Revision != in.ExpectedRevision {
		return Run{}, auth.ErrConflict
	}
	if state == "running" && d.Killed {
		return Run{}, auth.ErrConflict
	}
	if r.State == "running" && (d.Experiment == nil || d.Experiment.RunID != id) {
		return Run{}, auth.ErrConflict
	}
	if state == "running" {
		d.Experiment = r.Definition.Experiment
	} else {
		d.Experiment = nil
	}
	before := d.Revision
	d.Revision++
	if err = flags.AppendRevision(ctx, tx, actor, &d); err != nil {
		return Run{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE experiment_runs SET state=$2,started_at=CASE WHEN $2='running' THEN COALESCE(started_at,now()) ELSE started_at END,completed_at=CASE WHEN $2='completed' THEN now() ELSE completed_at END WHERE id=$1`, id, state)
	if err != nil {
		return Run{}, err
	}
	details, _ := json.Marshal(map[string]string{"run_id": id, "from": r.State, "to": state})
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: "human", ProjectID: projectID, EnvironmentID: d.EnvironmentID, Action: "experiment." + state, RequestID: requestID, Reason: in.Reason, BeforeRevision: &before, AfterRevision: &d.Revision, Details: details}); err != nil {
		return Run{}, err
	}
	r, err = readRun(ctx, tx, projectID, id)
	if err != nil {
		return Run{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Run{}, err
	}
	return r, nil
}
