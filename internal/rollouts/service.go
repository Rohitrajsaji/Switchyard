// Package rollouts implements approved, bounded traffic progression for a running experiment:
// reviewed plans, scheduled and metric-driven steps, guardrail checks and the safety rollback.
package rollouts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/audit"
	"switchyard/internal/auth"
	"switchyard/internal/flags"
	"switchyard/internal/metrics"
	"switchyard/internal/platform/identity"
	"switchyard/pkg/evaluation"
)

var (
	ErrNotFound = errors.New("rollout plan not found")
	ErrStale    = errors.New("rollout plan is stale")
	ErrExpired  = errors.New("rollout plan approval expired")
)

// ApprovalTTL is how long an approved plan may wait to be started (plan default: 24 hours).
const ApprovalTTL = 24 * time.Hour

// MaxStepIncreaseBP bounds each step to 10 percentage points (plan default).
const MaxStepIncreaseBP = 1000
const maxSteps = 20
const maxOffsetSeconds = 7 * 24 * 3600

// SystemActor attributes automated progression and rollback. It is an inactive database user.
var SystemActor = auth.Actor{ID: "system:rollout", Role: "developer"}

type Guardrails struct {
	WindowSeconds       int                  `json:"window_seconds"`
	MinRequests         int                  `json:"min_requests"`
	MaxErrorRate        float64              `json:"max_error_rate"`
	MaxP95MS            float64              `json:"max_p95_ms"`
	ConsecutiveBreaches int                  `json:"consecutive_breaches"`
	CooldownSeconds     int                  `json:"cooldown_seconds"`
	MaxCheckAgeSeconds  int                  `json:"max_check_age_seconds"`
	Conversion          *ConversionGuardrail `json:"conversion,omitempty"`
}
type ConversionGuardrail struct {
	MinExposed         int64   `json:"min_exposed"`
	MaxRelativeDecline float64 `json:"max_relative_decline"`
}

// DefaultGuardrails are the plan's tunable engineering defaults, not scientific thresholds.
func DefaultGuardrails() Guardrails {
	return Guardrails{WindowSeconds: 300, MinRequests: 1000, MaxErrorRate: 0.02, MaxP95MS: 500, ConsecutiveBreaches: 2, CooldownSeconds: 3600, MaxCheckAgeSeconds: 30}
}
func (g Guardrails) valid() bool {
	ok := g.WindowSeconds >= 60 && g.WindowSeconds <= 3600 && g.MinRequests >= 1 && g.MinRequests <= 1000000 &&
		g.MaxErrorRate > 0 && g.MaxErrorRate <= 1 && g.MaxP95MS >= 1 && g.MaxP95MS <= 60000 &&
		g.ConsecutiveBreaches >= 1 && g.ConsecutiveBreaches <= 10 && g.CooldownSeconds >= 60 && g.CooldownSeconds <= 604800 &&
		g.MaxCheckAgeSeconds >= 1 && g.MaxCheckAgeSeconds <= 600
	if g.Conversion != nil {
		ok = ok && g.Conversion.MinExposed >= 1 && g.Conversion.MaxRelativeDecline > 0 && g.Conversion.MaxRelativeDecline <= 1
	}
	return ok
}

type StepInput struct {
	TrafficBP     int `json:"traffic_bp"`
	OffsetSeconds int `json:"offset_seconds"`
}
type Step struct {
	Ordinal         int        `json:"ordinal"`
	TrafficBP       int        `json:"traffic_bp"`
	OffsetSeconds   int        `json:"offset_seconds"`
	DueAt           *time.Time `json:"due_at,omitempty"`
	State           string     `json:"state"`
	AppliedRevision *int64     `json:"applied_revision,omitempty"`
	AppliedAt       *time.Time `json:"applied_at,omitempty"`
}
type Plan struct {
	ID                string     `json:"id"`
	ProjectID         string     `json:"project_id"`
	EnvironmentID     string     `json:"environment_id"`
	FlagKey           string     `json:"flag_key"`
	RunID             string     `json:"run_id"`
	Mode              string     `json:"mode"`
	CeilingBP         int        `json:"ceiling_bp"`
	BaseRevision      int64      `json:"base_revision"`
	ExpectedRevision  *int64     `json:"expected_revision,omitempty"`
	Guardrails        Guardrails `json:"guardrails"`
	PlanHash          string     `json:"plan_hash"`
	Rationale         string     `json:"rationale"`
	ProposerID        string     `json:"proposer_id"`
	State             string     `json:"state"`
	ApproverID        *string    `json:"approver_id,omitempty"`
	ApprovedAt        *time.Time `json:"approved_at,omitempty"`
	ApprovalExpiresAt *time.Time `json:"approval_expires_at,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	FinishReason      string     `json:"finish_reason"`
	CreatedAt         time.Time  `json:"created_at"`
	Steps             []Step     `json:"steps"`
}
type CreateInput struct {
	EnvironmentID string      `json:"environment_id"`
	RunID         string      `json:"run_id"`
	Mode          string      `json:"mode"`
	CeilingBP     int         `json:"ceiling_bp"`
	Steps         []StepInput `json:"steps"`
	Guardrails    *Guardrails `json:"guardrails,omitempty"`
	Rationale     string      `json:"rationale"`
}

type Service struct {
	pool    *pgxpool.Pool
	flags   *flags.Service
	metrics *metrics.Service
	now     func() time.Time
}

func New(pool *pgxpool.Pool, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, flags: flags.New(pool), metrics: metrics.New(pool, now), now: now}
}
func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

const planColumns = `id,project_id,environment_id,flag_key,run_id,mode,ceiling_bp,base_revision,expected_revision,guardrails,plan_hash,rationale,proposer_id,state,approver_id,approved_at,approval_expires_at,started_at,finished_at,finish_reason,created_at`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	var g []byte
	err := row.Scan(&p.ID, &p.ProjectID, &p.EnvironmentID, &p.FlagKey, &p.RunID, &p.Mode, &p.CeilingBP, &p.BaseRevision, &p.ExpectedRevision, &g, &p.PlanHash, &p.Rationale, &p.ProposerID, &p.State, &p.ApproverID, &p.ApprovedAt, &p.ApprovalExpiresAt, &p.StartedAt, &p.FinishedAt, &p.FinishReason, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrNotFound
	}
	if err != nil {
		return Plan{}, err
	}
	return p, json.Unmarshal(g, &p.Guardrails)
}

type queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadSteps(ctx context.Context, q queryer, p *Plan) error {
	rows, err := q.Query(ctx, `SELECT ordinal,traffic_bp,offset_seconds,due_at,state,applied_revision,applied_at FROM rollout_steps WHERE plan_id=$1 ORDER BY ordinal`, p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	p.Steps = make([]Step, 0)
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.Ordinal, &st.TrafficBP, &st.OffsetSeconds, &st.DueAt, &st.State, &st.AppliedRevision, &st.AppliedAt); err != nil {
			return err
		}
		p.Steps = append(p.Steps, st)
	}
	return rows.Err()
}
func load(ctx context.Context, q queryer, projectID, id string, lock bool) (Plan, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	var p Plan
	var err error
	if projectID == "" {
		p, err = scanPlan(q.QueryRow(ctx, `SELECT `+planColumns+` FROM rollout_plans WHERE id=$1`+suffix, id))
	} else {
		p, err = scanPlan(q.QueryRow(ctx, `SELECT `+planColumns+` FROM rollout_plans WHERE id=$1 AND project_id=$2`+suffix, id, projectID))
	}
	if err != nil {
		return Plan{}, err
	}
	return p, loadSteps(ctx, q, &p)
}

func hashPlan(p Plan) (string, error) {
	body, err := json.Marshal(struct {
		ProjectID, EnvironmentID, FlagKey, RunID, Mode string
		CeilingBP                                      int
		BaseRevision                                   int64
		Steps                                          []Step
		Guardrails                                     Guardrails
	}{p.ProjectID, p.EnvironmentID, p.FlagKey, p.RunID, p.Mode, p.CeilingBP, p.BaseRevision, p.Steps, p.Guardrails})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// validateSteps enforces strictly increasing offsets, bounded 10-point increases and the ceiling.
func validateSteps(currentBP, ceiling int, steps []StepInput) error {
	if len(steps) < 1 || len(steps) > maxSteps {
		return auth.ErrInvalid
	}
	previous, offset := currentBP, -1
	for _, st := range steps {
		if st.OffsetSeconds <= offset || st.OffsetSeconds > maxOffsetSeconds || st.TrafficBP <= previous || st.TrafficBP-previous > MaxStepIncreaseBP || st.TrafficBP > ceiling {
			return auth.ErrInvalid
		}
		previous, offset = st.TrafficBP, st.OffsetSeconds
	}
	return nil
}

// runContext loads the run's attached state; the caller holds no locks yet.
type runContext struct {
	Definition evaluation.Definition
	Control    string
	State      string
}

func readRun(ctx context.Context, q queryer, projectID, runID string) (runContext, error) {
	var body []byte
	var rc runContext
	err := q.QueryRow(ctx, `SELECT definition,control_variant_id,state FROM experiment_runs WHERE project_id=$1 AND id=$2`, projectID, runID).Scan(&body, &rc.Control, &rc.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return rc, ErrNotFound
	}
	if err != nil {
		return rc, err
	}
	return rc, json.Unmarshal(body, &rc.Definition)
}

func record(ctx context.Context, tx pgx.Tx, actor auth.Actor, source string, p Plan, action, reason, requestID string, extra map[string]any) error {
	details := map[string]any{"plan_id": p.ID, "run_id": p.RunID, "flag_key": p.FlagKey, "plan_hash": p.PlanHash}
	for k, v := range extra {
		details[k] = v
	}
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: source, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID, Action: action, RequestID: requestID, Reason: reason, Details: body})
}

// Create validates and stores a rollout plan for a running, unkilled experiment.
func (s *Service) Create(ctx context.Context, actor auth.Actor, projectID string, in CreateInput, requestID string) (Plan, error) {
	g := DefaultGuardrails()
	if in.Guardrails != nil {
		g = *in.Guardrails
	}
	if (in.Mode != "scheduled" && in.Mode != "metric") || len(in.Rationale) < 1 || len(in.Rationale) > 512 || in.CeilingBP < 1 || in.CeilingBP > 10000 || !g.valid() {
		return Plan{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = auth.AuthorizeEnv(ctx, tx, actor, projectID, in.EnvironmentID, true); err != nil {
		return Plan{}, err
	}
	rc, err := readRun(ctx, tx, projectID, in.RunID)
	if err != nil {
		return Plan{}, err
	}
	if rc.Definition.EnvironmentID != in.EnvironmentID {
		return Plan{}, auth.ErrInvalid
	}
	d, err := flags.LockCurrent(ctx, tx, projectID, in.EnvironmentID, rc.Definition.Key)
	if err != nil {
		return Plan{}, err
	}
	if rc.State != "running" || d.Killed || d.Experiment == nil || d.Experiment.RunID != in.RunID {
		return Plan{}, auth.ErrConflict
	}
	if err = validateSteps(d.Experiment.TrafficBP, in.CeilingBP, in.Steps); err != nil {
		return Plan{}, err
	}
	p := Plan{ID: identity.New("rlp_"), ProjectID: projectID, EnvironmentID: in.EnvironmentID, FlagKey: rc.Definition.Key, RunID: in.RunID, Mode: in.Mode,
		CeilingBP: in.CeilingBP, BaseRevision: d.Revision, Guardrails: g, Rationale: in.Rationale, ProposerID: actor.ID, State: "proposed", CreatedAt: s.clock()}
	for i, st := range in.Steps {
		p.Steps = append(p.Steps, Step{Ordinal: i + 1, TrafficBP: st.TrafficBP, OffsetSeconds: st.OffsetSeconds, State: "pending"})
	}
	if p.PlanHash, err = hashPlan(p); err != nil {
		return Plan{}, err
	}
	gj, err := json.Marshal(p.Guardrails)
	if err != nil {
		return Plan{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO rollout_plans(id,project_id,environment_id,flag_key,run_id,mode,ceiling_bp,base_revision,guardrails,plan_hash,rationale,proposer_id,state,created_at)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'proposed',$13)`, p.ID, p.ProjectID, p.EnvironmentID, p.FlagKey, p.RunID, p.Mode, p.CeilingBP, p.BaseRevision, gj, p.PlanHash, p.Rationale, p.ProposerID, p.CreatedAt); err != nil {
		if isUnique(err) {
			return Plan{}, auth.ErrConflict // another live plan already drives this run
		}
		return Plan{}, err
	}
	for _, st := range p.Steps {
		if _, err = tx.Exec(ctx, `INSERT INTO rollout_steps(plan_id,ordinal,traffic_bp,offset_seconds) VALUES($1,$2,$3,$4)`, p.ID, st.Ordinal, st.TrafficBP, st.OffsetSeconds); err != nil {
			return Plan{}, err
		}
	}
	if err = record(ctx, tx, actor, "human", p, "rollout.proposed", p.Rationale, requestID, nil); err != nil {
		return Plan{}, err
	}
	return p, tx.Commit(ctx)
}

func isUnique(err error) bool {
	var pg interface{ SQLState() string }
	return errors.As(err, &pg) && pg.SQLState() == "23505"
}

// finish settles a plan into a final state, cancels pending steps and audits it. The caller commits.
func (s *Service) finish(ctx context.Context, tx pgx.Tx, actor auth.Actor, source string, p Plan, state, action, reason, requestID string, extra map[string]any) error {
	now := s.clock()
	if _, err := tx.Exec(ctx, `UPDATE rollout_plans SET state=$2,finished_at=$3,finish_reason=$4 WHERE id=$1`, p.ID, state, now, reason); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE rollout_steps SET state='cancelled' WHERE plan_id=$1 AND state='pending'`, p.ID); err != nil {
		return err
	}
	return record(ctx, tx, actor, source, p, action, reason, requestID, extra)
}

// Approve records a different admin's approval of the exact plan hash.
func (s *Service) Approve(ctx context.Context, actor auth.Actor, projectID, id, planHash, reason, requestID string) (Plan, error) {
	if len(reason) < 1 || len(reason) > 512 {
		return Plan{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	probe, err := load(ctx, tx, projectID, id, false)
	if err != nil {
		return Plan{}, err
	}
	if _, err = auth.AuthorizeEnv(ctx, tx, actor, projectID, probe.EnvironmentID, true); err != nil {
		return Plan{}, err
	}
	if err = auth.RequireRole(ctx, tx, actor, "admin"); err != nil {
		return Plan{}, err
	}
	if actor.ID == probe.ProposerID {
		return Plan{}, auth.ErrForbidden
	}
	// Lock order everywhere: flag row, then plan row.
	d, err := flags.LockCurrent(ctx, tx, projectID, probe.EnvironmentID, probe.FlagKey)
	if err != nil {
		return Plan{}, err
	}
	p, err := load(ctx, tx, projectID, id, true)
	if err != nil {
		return Plan{}, err
	}
	if p.State != "proposed" || planHash != p.PlanHash {
		return Plan{}, auth.ErrConflict
	}
	rc, err := readRun(ctx, tx, projectID, p.RunID)
	if err != nil {
		return Plan{}, err
	}
	if d.Revision != p.BaseRevision || rc.State != "running" || d.Killed || d.Experiment == nil || d.Experiment.RunID != p.RunID {
		if err = s.finish(ctx, tx, actor, "human", p, "stale", "rollout.stale", "configuration changed before approval", requestID, nil); err != nil {
			return Plan{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return Plan{}, err
		}
		return Plan{}, ErrStale
	}
	now := s.clock()
	if _, err = tx.Exec(ctx, `UPDATE rollout_plans SET state='approved',approver_id=$2,approved_at=$3,approval_expires_at=$4 WHERE id=$1`, p.ID, actor.ID, now, now.Add(ApprovalTTL)); err != nil {
		return Plan{}, err
	}
	if err = record(ctx, tx, actor, "human", p, "rollout.approved", reason, requestID, nil); err != nil {
		return Plan{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Plan{}, err
	}
	return s.Get(ctx, actor, projectID, id)
}

// Start begins execution of an approved plan within its approval window. Step due times are
// measured from this moment; a changed base revision makes the plan stale.
func (s *Service) Start(ctx context.Context, actor auth.Actor, projectID, id, requestID string) (Plan, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	probe, err := load(ctx, tx, projectID, id, false)
	if err != nil {
		return Plan{}, err
	}
	if _, err = auth.AuthorizeEnv(ctx, tx, actor, projectID, probe.EnvironmentID, true); err != nil {
		return Plan{}, err
	}
	d, err := flags.LockCurrent(ctx, tx, projectID, probe.EnvironmentID, probe.FlagKey)
	if err != nil {
		return Plan{}, err
	}
	p, err := load(ctx, tx, projectID, id, true)
	if err != nil {
		return Plan{}, err
	}
	switch p.State {
	case "running":
		return p, nil // idempotent
	case "approved":
	case "stale":
		return Plan{}, ErrStale
	case "expired":
		return Plan{}, ErrExpired
	default:
		return Plan{}, auth.ErrConflict
	}
	now := s.clock()
	if p.ApprovalExpiresAt == nil || !now.Before(*p.ApprovalExpiresAt) {
		if err = s.finish(ctx, tx, actor, "human", p, "expired", "rollout.expired", "approval expired before start", requestID, nil); err != nil {
			return Plan{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return Plan{}, err
		}
		return Plan{}, ErrExpired
	}
	if p.ApproverID == nil {
		return Plan{}, auth.ErrForbidden
	}
	if err = auth.RequireRole(ctx, tx, auth.Actor{ID: *p.ApproverID}, "admin"); err != nil {
		return Plan{}, err
	}
	rc, err := readRun(ctx, tx, projectID, p.RunID)
	if err != nil {
		return Plan{}, err
	}
	if d.Revision != p.BaseRevision || rc.State != "running" || d.Killed || d.Experiment == nil || d.Experiment.RunID != p.RunID {
		if err = s.finish(ctx, tx, actor, "human", p, "stale", "rollout.stale", "configuration changed before start", requestID, nil); err != nil {
			return Plan{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return Plan{}, err
		}
		return Plan{}, ErrStale
	}
	if _, err = tx.Exec(ctx, `UPDATE rollout_plans SET state='running',started_at=$2,expected_revision=$3 WHERE id=$1`, p.ID, now, d.Revision); err != nil {
		return Plan{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE rollout_steps SET due_at=$2::timestamptz + make_interval(secs => offset_seconds) WHERE plan_id=$1`, p.ID, now); err != nil {
		return Plan{}, err
	}
	if err = record(ctx, tx, actor, "human", p, "rollout.started", p.Rationale, requestID, nil); err != nil {
		return Plan{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Plan{}, err
	}
	return s.Get(ctx, actor, projectID, id)
}

// Reject closes a proposed or approved plan before it starts (admin or proposer).
func (s *Service) Reject(ctx context.Context, actor auth.Actor, projectID, id, reason, requestID string) (Plan, error) {
	return s.stop(ctx, actor, projectID, id, reason, requestID, "rejected", "rollout.rejected", []string{"proposed", "approved"})
}

// Cancel stops an approved or running plan. Applied steps stay in effect; no further
// step runs. Cancelling is allowed to any writer: it can only reduce automation.
func (s *Service) Cancel(ctx context.Context, actor auth.Actor, projectID, id, reason, requestID string) (Plan, error) {
	return s.stop(ctx, actor, projectID, id, reason, requestID, "cancelled", "rollout.cancelled", []string{"approved", "running"})
}
func (s *Service) stop(ctx context.Context, actor auth.Actor, projectID, id, reason, requestID, state, action string, from []string) (Plan, error) {
	if len(reason) < 1 || len(reason) > 512 {
		return Plan{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Plan{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	probe, err := load(ctx, tx, projectID, id, false)
	if err != nil {
		return Plan{}, err
	}
	if _, err = auth.AuthorizeEnv(ctx, tx, actor, projectID, probe.EnvironmentID, true); err != nil {
		return Plan{}, err
	}
	if state == "rejected" && actor.ID != probe.ProposerID {
		if err = auth.RequireRole(ctx, tx, actor, "admin"); err != nil {
			return Plan{}, err
		}
	}
	if _, err = flags.LockCurrent(ctx, tx, projectID, probe.EnvironmentID, probe.FlagKey); err != nil {
		return Plan{}, err
	}
	p, err := load(ctx, tx, projectID, id, true)
	if err != nil {
		return Plan{}, err
	}
	allowed := false
	for _, f := range from {
		allowed = allowed || p.State == f
	}
	if !allowed {
		return Plan{}, auth.ErrConflict
	}
	if err = s.finish(ctx, tx, actor, "human", p, state, action, reason, requestID, nil); err != nil {
		return Plan{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Plan{}, err
	}
	return s.Get(ctx, actor, projectID, id)
}

func (s *Service) Get(ctx context.Context, actor auth.Actor, projectID, id string) (Plan, error) {
	if err := auth.Authorize(ctx, s.pool, actor, projectID, "", false); err != nil {
		return Plan{}, err
	}
	return load(ctx, s.pool, projectID, id, false)
}

// List returns up to 100 plans, newest first, optionally for one environment.
func (s *Service) List(ctx context.Context, actor auth.Actor, projectID, environmentID string) ([]Plan, error) {
	if err := auth.Authorize(ctx, s.pool, actor, projectID, environmentID, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id FROM rollout_plans WHERE project_id=$1 AND ($2='' OR environment_id=$2) ORDER BY created_at DESC,id LIMIT 100`, projectID, environmentID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	plans := make([]Plan, 0, len(ids))
	for _, id := range ids {
		p, err := load(ctx, s.pool, projectID, id, false)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, nil
}

type Check struct {
	ID        int64           `json:"id"`
	CheckedAt time.Time       `json:"checked_at"`
	Decision  string          `json:"decision"`
	Evidence  json.RawMessage `json:"evidence"`
}

// Checks returns the newest guardrail checks (evidence) for a plan.
func (s *Service) Checks(ctx context.Context, actor auth.Actor, projectID, id string) ([]Check, error) {
	if err := auth.Authorize(ctx, s.pool, actor, projectID, "", false); err != nil {
		return nil, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rollout_plans WHERE id=$1 AND project_id=$2)`, id, projectID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT id,checked_at,decision,evidence FROM guardrail_checks WHERE plan_id=$1 ORDER BY id DESC LIMIT 50`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Check, 0)
	for rows.Next() {
		var c Check
		if err := rows.Scan(&c.ID, &c.CheckedAt, &c.Decision, &c.Evidence); err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

// TrafficInput is a manual, immediate traffic change for a running experiment.
type TrafficInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	TrafficBP        int    `json:"traffic_bp"`
	Reason           string `json:"reason"`
}

// SetTraffic applies a manual traffic change. Development and staging accept raises of at most
// 10 points; production accepts only decreases (an emergency reduction). While a live plan
// drives the run only a decrease is accepted, and that decrease stops the plan at its next
// check because the revision no longer matches what the plan approved.
func (s *Service) SetTraffic(ctx context.Context, actor auth.Actor, projectID, runID string, in TrafficInput, requestID string) (evaluation.Definition, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return evaluation.Definition{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var environmentID string
	if err = tx.QueryRow(ctx, `SELECT environment_id FROM experiment_runs WHERE project_id=$1 AND id=$2`, projectID, runID).Scan(&environmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evaluation.Definition{}, ErrNotFound
		}
		return evaluation.Definition{}, err
	}
	env, err := auth.AuthorizeEnv(ctx, tx, actor, projectID, environmentID, true)
	if err != nil {
		return evaluation.Definition{}, err
	}
	rc, err := readRun(ctx, tx, projectID, runID)
	if err != nil {
		return evaluation.Definition{}, err
	}
	current, err := flags.LockCurrent(ctx, tx, projectID, environmentID, rc.Definition.Key)
	if err != nil {
		return evaluation.Definition{}, err
	}
	if current.Experiment == nil || current.Experiment.RunID != runID {
		return evaluation.Definition{}, auth.ErrConflict
	}
	increase := in.TrafficBP > current.Experiment.TrafficBP
	if in.TrafficBP == current.Experiment.TrafficBP || (increase && (env == "production" || in.TrafficBP-current.Experiment.TrafficBP > MaxStepIncreaseBP)) {
		return evaluation.Definition{}, auth.ErrInvalid
	}
	if increase {
		var live bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM rollout_plans WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND state IN ('approved','running'))`, projectID, environmentID, runID).Scan(&live); err != nil {
			return evaluation.Definition{}, err
		}
		if live {
			return evaluation.Definition{}, auth.ErrConflict
		}
	}
	d, err := s.flags.ApplyTraffic(ctx, tx, actor, projectID, flags.TrafficChange{EnvironmentID: environmentID, Key: rc.Definition.Key, ExpectedRevision: in.ExpectedRevision, TrafficBP: in.TrafficBP, Reason: in.Reason}, requestID)
	if err != nil {
		return evaluation.Definition{}, err
	}
	return d, tx.Commit(ctx)
}
