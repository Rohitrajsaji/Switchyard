// Package proposals implements reviewed production configuration changes: exact-diff
// proposals, separate-person approval with expiry, and idempotent revision-checked application.
package proposals

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
	"switchyard/internal/platform/identity"
	"switchyard/internal/rollouts"
	"switchyard/pkg/evaluation"
)

var (
	ErrNotFound = errors.New("proposal not found")
	// ErrStale means the configuration moved (or no longer validates) since the proposal's base.
	ErrStale = errors.New("proposal is stale")
	// ErrExpired means the approval outlived ApprovalTTL before it was applied.
	ErrExpired = errors.New("proposal approval expired")
	// ErrCooldown means a safety rollback of this flag is still cooling down; re-enabling waits.
	ErrCooldown = errors.New("flag is cooling down after a safety rollback")
)

// ApprovalTTL is how long an approval authorizes application (plan default: 24 hours).
const ApprovalTTL = 24 * time.Hour

type Proposal struct {
	ID                string              `json:"id"`
	ProjectID         string              `json:"project_id"`
	EnvironmentID     string              `json:"environment_id"`
	FlagKey           string              `json:"flag_key"`
	Kind              string              `json:"kind"`
	FlagType          string              `json:"type"`
	BaseRevision      int64               `json:"base_revision"`
	Configuration     flags.Configuration `json:"configuration"`
	Diff              json.RawMessage     `json:"diff"`
	DiffHash          string              `json:"diff_hash"`
	Rationale         string              `json:"rationale"`
	ProposerID        string              `json:"proposer_id"`
	Source            string              `json:"source"`
	State             string              `json:"state"`
	ApproverID        *string             `json:"approver_id,omitempty"`
	ApprovedAt        *time.Time          `json:"approved_at,omitempty"`
	ApprovalExpiresAt *time.Time          `json:"approval_expires_at,omitempty"`
	AppliedRevision   *int64              `json:"applied_revision,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
	DecidedAt         *time.Time          `json:"decided_at,omitempty"`
}
type CreateInput struct {
	EnvironmentID string `json:"environment_id"`
	FlagKey       string `json:"key"`
	Kind          string `json:"kind"` // "create" or "update"
	FlagType      string `json:"type"` // required for create
	BaseRevision  int64  `json:"expected_revision"`
	flags.Configuration
	Rationale string `json:"rationale"`
}

type Service struct {
	pool  *pgxpool.Pool
	flags *flags.Service
	now   func() time.Time
}

func New(pool *pgxpool.Pool, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{pool: pool, flags: flags.New(pool), now: now}
}

const columns = `id,project_id,environment_id,flag_key,kind,flag_type,base_revision,configuration,diff,diff_hash,rationale,proposer_id,source,state,approver_id,approved_at,approval_expires_at,applied_revision,created_at,decided_at`

func scan(row pgx.Row) (Proposal, error) {
	var p Proposal
	var config []byte
	err := row.Scan(&p.ID, &p.ProjectID, &p.EnvironmentID, &p.FlagKey, &p.Kind, &p.FlagType, &p.BaseRevision, &config, &p.Diff, &p.DiffHash, &p.Rationale, &p.ProposerID, &p.Source, &p.State, &p.ApproverID, &p.ApprovedAt, &p.ApprovalExpiresAt, &p.AppliedRevision, &p.CreatedAt, &p.DecidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, ErrNotFound
	}
	if err != nil {
		return Proposal{}, err
	}
	return p, json.Unmarshal(config, &p.Configuration)
}

func configurationOf(d evaluation.Definition) flags.Configuration {
	return flags.Configuration{Default: d.Default, Safe: d.Safe, Rules: d.Rules, Rollout: d.Rollout, Killed: d.Killed}
}

// diffOf is the exact reviewed change. The hash binds scope, kind, base revision and both
// canonical configurations, so an approval cannot be reused for any other change.
func diffOf(p Proposal, old *evaluation.Definition, next evaluation.Definition) (json.RawMessage, string, error) {
	var before *flags.Configuration
	if old != nil {
		c := configurationOf(*old)
		before = &c
	}
	body, err := json.Marshal(struct {
		ProjectID     string               `json:"project_id"`
		EnvironmentID string               `json:"environment_id"`
		FlagKey       string               `json:"flag_key"`
		Kind          string               `json:"kind"`
		BaseRevision  int64                `json:"base_revision"`
		Before        *flags.Configuration `json:"before"`
		After         flags.Configuration  `json:"after"`
	}{p.ProjectID, p.EnvironmentID, p.FlagKey, p.Kind, p.BaseRevision, before, configurationOf(next)})
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}

func (p Proposal) change(reason string) flags.Change {
	return flags.Change{Create: p.Kind == "create", EnvironmentID: p.EnvironmentID, Key: p.FlagKey, Type: p.FlagType, ExpectedRevision: p.BaseRevision, Configuration: p.Configuration, Reason: reason}
}

// dryRun validates the change through the same code as real application, then discards it.
func (s *Service) dryRun(ctx context.Context, tx pgx.Tx, actor auth.Actor, p Proposal, requestID string) (json.RawMessage, string, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, "", err
	}
	old, next, err := s.flags.ApplyTx(ctx, sp, actor, p.ProjectID, p.change(p.Rationale), requestID, flags.Options{})
	_ = sp.Rollback(ctx)
	if err != nil {
		return nil, "", err
	}
	return diffOf(p, old, next)
}

func validation(err error) bool {
	return errors.Is(err, auth.ErrConflict) || errors.Is(err, auth.ErrInvalid) || errors.Is(err, flags.ErrNotFound)
}

func record(ctx context.Context, tx pgx.Tx, actor auth.Actor, p Proposal, action, reason, requestID, source string) error {
	if source != "agent" {
		source = "human"
	}
	details, err := json.Marshal(map[string]any{"proposal_id": p.ID, "flag_key": p.FlagKey, "diff_hash": p.DiffHash, "proposer_id": p.ProposerID, "proposal_source": p.Source})
	if err != nil {
		return err
	}
	return audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: source, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID, Action: action, RequestID: requestID, Reason: reason, Details: details})
}

// Create validates and stores a production proposal. Development and staging are edited directly.
func (s *Service) Create(ctx context.Context, actor auth.Actor, projectID string, in CreateInput, requestID string) (Proposal, error) {
	if (in.Kind != "create" && in.Kind != "update") || in.FlagKey == "" || len(in.Rationale) < 1 || len(in.Rationale) > 512 || in.BaseRevision < 0 || (in.Kind == "create" && in.BaseRevision != 0) {
		return Proposal{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	env, err := auth.AuthorizeEnv(ctx, tx, actor, projectID, in.EnvironmentID, true)
	if err != nil {
		return Proposal{}, err
	}
	if env != "production" {
		return Proposal{}, auth.ErrInvalid
	}
	kind := in.FlagType
	if in.Kind == "update" {
		if err := tx.QueryRow(ctx, `SELECT type FROM flags WHERE project_id=$1 AND key=$2`, projectID, in.FlagKey).Scan(&kind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Proposal{}, flags.ErrNotFound
			}
			return Proposal{}, err
		}
	}
	p := Proposal{ID: identity.New("prp_"), ProjectID: projectID, EnvironmentID: in.EnvironmentID, FlagKey: in.FlagKey, Kind: in.Kind, FlagType: kind,
		BaseRevision: in.BaseRevision, Configuration: in.Configuration, Rationale: in.Rationale, ProposerID: actor.ID, Source: "human", State: "validated", CreatedAt: s.now().UTC().Truncate(time.Microsecond)}
	return s.store(ctx, tx, actor, p, requestID)
}

// CreateAgent submits a production proposal for the human who created the application key.
// The model cannot choose the proposer, approve, or apply. A standalone rollout may rise by
// at most MaxAgentIncreaseBP in one proposal.
func (s *Service) CreateAgent(ctx context.Context, token, projectID string, in CreateInput, requestID string) (Proposal, error) {
	if (in.Kind != "create" && in.Kind != "update") || in.FlagKey == "" || len(in.Rationale) < 1 || len(in.Rationale) > 512 || in.BaseRevision < 0 || (in.Kind == "create" && in.BaseRevision != 0) {
		return Proposal{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	app, err := auth.AuthorizeApplication(ctx, tx, token, projectID, in.EnvironmentID, "proposals:submit")
	if err != nil {
		return Proposal{}, err
	}
	var actor auth.Actor
	err = tx.QueryRow(ctx, `SELECT u.id, u.email, u.role FROM application_keys k JOIN users u ON u.id = k.created_by WHERE k.id=$1 AND u.active`, app.ID).Scan(&actor.ID, &actor.Email, &actor.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Proposal{}, auth.ErrUnauthorized
	}
	if err != nil {
		return Proposal{}, err
	}
	env, err := auth.AuthorizeEnv(ctx, tx, actor, projectID, in.EnvironmentID, true)
	if err != nil {
		return Proposal{}, err
	}
	if env != "production" {
		return Proposal{}, auth.ErrInvalid
	}
	kind := in.FlagType
	if in.Kind == "update" {
		if err := tx.QueryRow(ctx, `SELECT type FROM flags WHERE project_id=$1 AND key=$2`, projectID, in.FlagKey).Scan(&kind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Proposal{}, flags.ErrNotFound
			}
			return Proposal{}, err
		}
	}
	p := Proposal{ID: identity.New("prp_"), ProjectID: projectID, EnvironmentID: in.EnvironmentID, FlagKey: in.FlagKey, Kind: in.Kind, FlagType: kind,
		BaseRevision: in.BaseRevision, Configuration: in.Configuration, Rationale: in.Rationale, ProposerID: actor.ID, Source: "agent", State: "validated", CreatedAt: s.now().UTC().Truncate(time.Microsecond)}
	return s.store(ctx, tx, actor, p, requestID)
}

func (s *Service) store(ctx context.Context, tx pgx.Tx, actor auth.Actor, p Proposal, requestID string) (Proposal, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	old, next, err := s.flags.ApplyTx(ctx, sp, actor, p.ProjectID, p.change(p.Rationale), requestID, flags.Options{})
	_ = sp.Rollback(ctx)
	if err != nil {
		return Proposal{}, err
	}
	// The stored configuration is the canonical validated form, not the raw request.
	p.Configuration = configurationOf(next)
	if old != nil && configurationEqual(configurationOf(*old), p.Configuration) {
		return Proposal{}, auth.ErrInvalid // no-op proposals carry nothing to review
	}
	if p.Source == "agent" && trafficIncrease(old, next) > MaxAgentIncreaseBP {
		return Proposal{}, auth.ErrInvalid
	}
	if p.Diff, p.DiffHash, err = diffOf(p, old, next); err != nil {
		return Proposal{}, err
	}
	config, err := json.Marshal(p.Configuration)
	if err != nil {
		return Proposal{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO proposals(`+columns+`) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,NULL,NULL,NULL,NULL,$15,NULL)`,
		p.ID, p.ProjectID, p.EnvironmentID, p.FlagKey, p.Kind, p.FlagType, p.BaseRevision, config, p.Diff, p.DiffHash, p.Rationale, p.ProposerID, p.Source, p.State, p.CreatedAt); err != nil {
		return Proposal{}, err
	}
	if err = record(ctx, tx, actor, p, "proposal.created", p.Rationale, requestID, p.Source); err != nil {
		return Proposal{}, err
	}
	return p, tx.Commit(ctx)
}

func configurationEqual(a, b flags.Configuration) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

func (s *Service) lock(ctx context.Context, tx pgx.Tx, projectID, id string) (Proposal, error) {
	return scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM proposals WHERE id=$1 AND project_id=$2 FOR UPDATE`, id, projectID))
}

// settle moves a proposal to a final non-applied state, audits it and commits.
func (s *Service) settle(ctx context.Context, tx pgx.Tx, actor auth.Actor, p Proposal, state, action, reason, requestID string) error {
	if _, err := tx.Exec(ctx, `UPDATE proposals SET state=$2,decided_at=$3 WHERE id=$1`, p.ID, state, s.now().UTC().Truncate(time.Microsecond)); err != nil {
		return err
	}
	if err := record(ctx, tx, actor, p, action, reason, requestID, "human"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Approve records a different admin's approval of the exact diff identified by diffHash.
func (s *Service) Approve(ctx context.Context, actor auth.Actor, projectID, id, diffHash, reason, requestID string) (Proposal, error) {
	if len(reason) < 1 || len(reason) > 512 {
		return Proposal{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	p, err := s.lock(ctx, tx, projectID, id)
	if err != nil {
		return Proposal{}, err
	}
	if _, err = auth.AuthorizeEnv(ctx, tx, actor, projectID, p.EnvironmentID, true); err != nil {
		return Proposal{}, err
	}
	if err = auth.RequireRole(ctx, tx, actor, "admin"); err != nil {
		return Proposal{}, err
	}
	if actor.ID == p.ProposerID {
		return Proposal{}, auth.ErrForbidden // a second person must approve
	}
	if p.State != "validated" || diffHash != p.DiffHash {
		return Proposal{}, auth.ErrConflict
	}
	_, hash, err := s.dryRun(ctx, tx, actor, p, requestID)
	if err == nil && hash != p.DiffHash {
		err = auth.ErrConflict
	}
	if err != nil {
		if validation(err) {
			if e := s.settle(ctx, tx, actor, p, "stale", "proposal.stale", "base revision or validation changed before approval", requestID); e != nil {
				return Proposal{}, e
			}
			return Proposal{}, ErrStale
		}
		return Proposal{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	if _, err = tx.Exec(ctx, `UPDATE proposals SET state='approved',approver_id=$2,approved_at=$3,approval_expires_at=$4 WHERE id=$1`, p.ID, actor.ID, now, now.Add(ApprovalTTL)); err != nil {
		return Proposal{}, err
	}
	if err = record(ctx, tx, actor, p, "proposal.approved", reason, requestID, "human"); err != nil {
		return Proposal{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Proposal{}, err
	}
	return s.Get(ctx, actor, projectID, id)
}

// Reject closes an open proposal. An admin or the proposer (withdrawal) may reject.
func (s *Service) Reject(ctx context.Context, actor auth.Actor, projectID, id, reason, requestID string) (Proposal, error) {
	if len(reason) < 1 || len(reason) > 512 {
		return Proposal{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	p, err := s.lock(ctx, tx, projectID, id)
	if err != nil {
		return Proposal{}, err
	}
	if _, err = auth.AuthorizeEnv(ctx, tx, actor, projectID, p.EnvironmentID, true); err != nil {
		return Proposal{}, err
	}
	if actor.ID != p.ProposerID {
		if err = auth.RequireRole(ctx, tx, actor, "admin"); err != nil {
			return Proposal{}, err
		}
	}
	if p.State != "validated" && p.State != "approved" {
		return Proposal{}, auth.ErrConflict
	}
	if err = s.settle(ctx, tx, actor, p, "rejected", "proposal.rejected", reason, requestID); err != nil {
		return Proposal{}, err
	}
	return s.Get(ctx, actor, projectID, id)
}

// Apply commits an approved proposal exactly once. Repeating it returns the original result.
func (s *Service) Apply(ctx context.Context, actor auth.Actor, projectID, id, requestID string) (Proposal, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Lock order is always proposal row then flag row.
	p, err := s.lock(ctx, tx, projectID, id)
	if err != nil {
		return Proposal{}, err
	}
	if _, err = auth.AuthorizeEnv(ctx, tx, actor, projectID, p.EnvironmentID, true); err != nil {
		return Proposal{}, err
	}
	switch p.State {
	case "applied":
		return p, nil
	case "approved":
	case "stale":
		return Proposal{}, ErrStale
	case "expired":
		return Proposal{}, ErrExpired
	default:
		return Proposal{}, auth.ErrConflict
	}
	now := s.now().UTC()
	if p.ApprovalExpiresAt == nil || !now.Before(*p.ApprovalExpiresAt) {
		if e := s.settle(ctx, tx, actor, p, "expired", "proposal.expired", "approval expired before application", requestID); e != nil {
			return Proposal{}, e
		}
		return Proposal{}, ErrExpired
	}
	// Approval authorizes the change only while the approver still holds the role.
	if p.ApproverID == nil {
		return Proposal{}, auth.ErrForbidden
	}
	if err = auth.RequireRole(ctx, tx, auth.Actor{ID: *p.ApproverID}, "admin"); err != nil {
		return Proposal{}, err
	}
	// A safety rollback starts a cooldown during which no approval may re-enable the flag.
	if !p.Configuration.Killed {
		cooling, err := rollouts.ActiveCooldown(ctx, tx, p.ProjectID, p.EnvironmentID, p.FlagKey, now)
		if err != nil {
			return Proposal{}, err
		}
		if cooling {
			return Proposal{}, ErrCooldown
		}
	}
	sp, err := tx.Begin(ctx)
	if err != nil {
		return Proposal{}, err
	}
	var applied int64
	gate := func(old *evaluation.Definition, next evaluation.Definition) error {
		_, hash, err := diffOf(p, old, next)
		if err != nil {
			return err
		}
		if hash != p.DiffHash {
			return auth.ErrConflict
		}
		applied = next.Revision
		return nil
	}
	_, _, err = s.flags.ApplyTx(ctx, sp, actor, projectID, p.change(p.Rationale), requestID, flags.Options{Gate: gate, Details: map[string]any{"proposal_id": p.ID, "proposer_id": p.ProposerID, "approver_id": *p.ApproverID, "diff_hash": p.DiffHash}})
	if err != nil {
		_ = sp.Rollback(ctx)
		if validation(err) {
			if e := s.settle(ctx, tx, actor, p, "stale", "proposal.stale", "base revision or validation changed before application", requestID); e != nil {
				return Proposal{}, e
			}
			return Proposal{}, ErrStale
		}
		return Proposal{}, err
	}
	if err = sp.Commit(ctx); err != nil {
		return Proposal{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE proposals SET state='applied',applied_revision=$2,decided_at=$3 WHERE id=$1`, p.ID, applied, now.Truncate(time.Microsecond)); err != nil {
		return Proposal{}, err
	}
	if err = record(ctx, tx, actor, p, "proposal.applied", p.Rationale, requestID, "human"); err != nil {
		return Proposal{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Proposal{}, err
	}
	return s.Get(ctx, actor, projectID, id)
}

func (s *Service) Get(ctx context.Context, actor auth.Actor, projectID, id string) (Proposal, error) {
	if err := auth.Authorize(ctx, s.pool, actor, projectID, "", false); err != nil {
		return Proposal{}, err
	}
	return scan(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM proposals WHERE id=$1 AND project_id=$2`, id, projectID))
}

// List returns up to 100 proposals, newest first, optionally for one environment.
func (s *Service) List(ctx context.Context, actor auth.Actor, projectID, environmentID string) ([]Proposal, error) {
	if err := auth.Authorize(ctx, s.pool, actor, projectID, environmentID, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM proposals WHERE project_id=$1 AND ($2='' OR environment_id=$2) ORDER BY created_at DESC,id LIMIT 100`, projectID, environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Proposal, 0)
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, p)
	}
	return items, rows.Err()
}
