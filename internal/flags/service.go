package flags

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/audit"
	"switchyard/internal/auth"
	"switchyard/internal/outbox"
	"switchyard/internal/platform/identity"
	"switchyard/pkg/evaluation"
)

var ErrNotFound = errors.New("flag not found")

type Configuration struct {
	Default evaluation.Value    `json:"default"`
	Safe    evaluation.Value    `json:"safe"`
	Rules   []evaluation.Rule   `json:"rules"`
	Rollout *evaluation.Rollout `json:"rollout,omitempty"`
	Killed  bool                `json:"killed"`
}
type CreateInput struct {
	EnvironmentID string `json:"environment_id"`
	Key           string `json:"key"`
	Type          string `json:"type"`
	Configuration
	Reason string `json:"reason"`
}
type UpdateInput struct {
	EnvironmentID    string `json:"environment_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Configuration
	Reason string `json:"reason"`
}
type Service struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func definition(projectID, environmentID, flagID, key, kind string, revision int64, c Configuration) evaluation.Definition {
	return evaluation.Definition{ProjectID: projectID, EnvironmentID: environmentID, FlagID: flagID, Key: key, Type: kind, Revision: revision, Default: c.Default, Safe: c.Safe, Rules: c.Rules, Rollout: c.Rollout, Killed: c.Killed}
}

// Change is one configuration mutation. Create adds the flag; otherwise it updates the
// environment's current configuration at ExpectedRevision.
type Change struct {
	Create           bool
	EnvironmentID    string
	Key, Type        string
	ExpectedRevision int64
	Configuration
	Reason string
}

// Options customize ApplyTx. Gate runs after the new definition is validated and canonical,
// before audit and commit; returning an error aborts the caller's transaction. Old is nil
// for a created flag or a first configuration in the environment.
type Options struct {
	Gate    func(old *evaluation.Definition, next evaluation.Definition) error
	Source  string
	Details map[string]any
}

func (s *Service) Create(ctx context.Context, actor auth.Actor, projectID string, in CreateInput, requestID string) (evaluation.Definition, error) {
	return s.direct(ctx, actor, projectID, Change{Create: true, EnvironmentID: in.EnvironmentID, Key: in.Key, Type: in.Type, Configuration: in.Configuration, Reason: in.Reason}, requestID)
}
func (s *Service) Update(ctx context.Context, actor auth.Actor, projectID, key string, in UpdateInput, requestID string) (evaluation.Definition, error) {
	return s.direct(ctx, actor, projectID, Change{EnvironmentID: in.EnvironmentID, Key: key, ExpectedRevision: in.ExpectedRevision, Configuration: in.Configuration, Reason: in.Reason}, requestID)
}

// direct applies a change requested without an approved proposal. Production accepts only
// the emergency-exempt reductions defined by Exempt.
func (s *Service) direct(ctx context.Context, actor auth.Actor, projectID string, ch Change, requestID string) (evaluation.Definition, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return evaluation.Definition{}, err
	}
	defer rollback(tx)
	env, err := auth.AuthorizeEnv(ctx, tx, actor, projectID, ch.EnvironmentID, true)
	if err != nil {
		return evaluation.Definition{}, err
	}
	opt := Options{}
	if env == "production" {
		opt.Details = map[string]any{"policy": "emergency_exempt"}
		opt.Gate = func(old *evaluation.Definition, next evaluation.Definition) error {
			if ch.Create || old == nil || !Exempt(*old, next) {
				return auth.ErrForbidden
			}
			return nil
		}
	}
	_, d, err := s.ApplyTx(ctx, tx, actor, projectID, ch, requestID, opt)
	if err != nil {
		return evaluation.Definition{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return evaluation.Definition{}, err
	}
	return d, nil
}

// Exempt reports whether next is an emergency reduction of old that needs no review: an
// explicit kill (safe-value disable) or a standalone-rollout traffic decrease, with every
// other field unchanged. Both arguments must be canonical stored definitions. Re-enabling,
// raising traffic and any value/targeting change are never exempt.
func Exempt(old, next evaluation.Definition) bool {
	a, b := old, next
	a.Revision, b.Revision = 0, 0
	switch {
	case !old.Killed && next.Killed:
		a.Killed, b.Killed = false, false
	case old.Killed == next.Killed && old.Rollout != nil && next.Rollout != nil && next.Rollout.TrafficBP < old.Rollout.TrafficBP:
		ra, rb := *old.Rollout, *next.Rollout
		ra.TrafficBP, rb.TrafficBP = 0, 0
		a.Rollout, b.Rollout = &ra, &rb
	default:
		return false
	}
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ja, jb)
}

// ApplyTx validates and persists one change inside the caller's transaction. The caller owns
// authorization, policy, commit and rollback; use a transaction it will roll back for a
// dry run. All revision-conflict, frozen-population and compilation checks live here, so
// direct edits and approved proposals cannot diverge.
func (s *Service) ApplyTx(ctx context.Context, tx pgx.Tx, actor auth.Actor, projectID string, ch Change, requestID string, opt Options) (*evaluation.Definition, evaluation.Definition, error) {
	configuration, err := normalize(ch.Configuration)
	if err != nil {
		return nil, evaluation.Definition{}, err
	}
	ch.Configuration = configuration
	if len(ch.Reason) < 1 || len(ch.Reason) > 512 || ch.ExpectedRevision < 0 {
		return nil, evaluation.Definition{}, auth.ErrInvalid
	}
	source := opt.Source
	if source == "" {
		source = "human"
	}
	details := map[string]any{"flag_key": ch.Key}
	for k, v := range opt.Details {
		details[k] = v
	}
	detailJSON, err := json.Marshal(details)
	if err != nil {
		return nil, evaluation.Definition{}, err
	}
	if ch.Create {
		d := definition(projectID, ch.EnvironmentID, identity.New("flg_"), ch.Key, ch.Type, 1, ch.Configuration)
		c, err := evaluation.Compile(d)
		if err != nil {
			return nil, evaluation.Definition{}, auth.ErrInvalid
		}
		if _, err = tx.Exec(ctx, `INSERT INTO flags(id,project_id,key,type) VALUES($1,$2,$3,$4)`, d.FlagID, projectID, d.Key, d.Type); err != nil {
			return nil, evaluation.Definition{}, classify(err)
		}
		if err = saveRevision(ctx, tx, actor, &d, c); err != nil {
			return nil, evaluation.Definition{}, err
		}
		if opt.Gate != nil {
			if err = opt.Gate(nil, d); err != nil {
				return nil, evaluation.Definition{}, err
			}
		}
		if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: source, ProjectID: projectID, EnvironmentID: ch.EnvironmentID, Action: "flag.created", RequestID: requestID, Reason: ch.Reason, AfterRevision: &d.Revision, Details: detailJSON}); err != nil {
			return nil, evaluation.Definition{}, err
		}
		return nil, d, nil
	}
	var flagID, kind string
	// One flag-level row lock also serializes first configuration in a new environment.
	err = tx.QueryRow(ctx, `SELECT id,type FROM flags WHERE project_id=$1 AND key=$2 FOR UPDATE`, projectID, ch.Key).Scan(&flagID, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, evaluation.Definition{}, ErrNotFound
	}
	if err != nil {
		return nil, evaluation.Definition{}, err
	}
	var before int64
	err = tx.QueryRow(ctx, `SELECT current_revision FROM environment_flag_state WHERE flag_id=$1 AND environment_id=$2`, flagID, ch.EnvironmentID).Scan(&before)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, evaluation.Definition{}, err
	}
	if before != ch.ExpectedRevision {
		return nil, evaluation.Definition{}, auth.ErrConflict
	}
	var reserved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM experiment_runs WHERE flag_id=$1 AND environment_id=$2 AND state<>'completed')`, flagID, ch.EnvironmentID).Scan(&reserved); err != nil {
		return nil, evaluation.Definition{}, err
	}
	d := definition(projectID, ch.EnvironmentID, flagID, ch.Key, kind, before+1, ch.Configuration)
	var old *evaluation.Definition
	if before > 0 {
		var body []byte
		if err := tx.QueryRow(ctx, `SELECT definition FROM flag_revisions WHERE flag_id=$1 AND environment_id=$2 AND revision=$3`, flagID, ch.EnvironmentID, before).Scan(&body); err != nil {
			return nil, evaluation.Definition{}, err
		}
		var stored evaluation.Definition
		if err := json.Unmarshal(body, &stored); err != nil {
			return nil, evaluation.Definition{}, err
		}
		old = &stored
		if reserved || stored.Experiment != nil {
			// A frozen population permits only an emergency disable, with every
			// other field unchanged. Preserve attached run metadata for history.
			if stored.Killed || !d.Killed {
				return nil, evaluation.Definition{}, auth.ErrConflict
			}
			d.Experiment = stored.Experiment
			candidate := d
			candidate.Revision, candidate.Killed = stored.Revision, stored.Killed
			compiled, err := evaluation.Compile(candidate)
			if err != nil {
				return nil, evaluation.Definition{}, auth.ErrInvalid
			}
			candidateBody, err := json.Marshal(compiled)
			if err != nil {
				return nil, evaluation.Definition{}, err
			}
			// Compare complete configurations as jsonb, not as a single flag
			// value: the definition may legitimately exceed a value's 16 KiB cap.
			var unchanged bool
			if err := tx.QueryRow(ctx, `SELECT $1::jsonb = $2::jsonb`, candidateBody, body).Scan(&unchanged); err != nil {
				return nil, evaluation.Definition{}, err
			}
			if !unchanged {
				return nil, evaluation.Definition{}, auth.ErrConflict
			}
		}
		// Salt changes would reshuffle users; changing the population requires a new flag/run.
		if stored.Rollout != nil && d.Rollout != nil && stored.Rollout.Salt != d.Rollout.Salt {
			return nil, evaluation.Definition{}, auth.ErrInvalid
		}
	}
	c, err := evaluation.Compile(d)
	if err != nil {
		return nil, evaluation.Definition{}, auth.ErrInvalid
	}
	if err = saveRevision(ctx, tx, actor, &d, c); err != nil {
		return nil, evaluation.Definition{}, err
	}
	if opt.Gate != nil {
		if err = opt.Gate(old, d); err != nil {
			return nil, evaluation.Definition{}, err
		}
	}
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: source, ProjectID: projectID, EnvironmentID: ch.EnvironmentID, Action: "flag.updated", RequestID: requestID, Reason: ch.Reason, BeforeRevision: &before, AfterRevision: &d.Revision, Details: detailJSON}); err != nil {
		return nil, evaluation.Definition{}, err
	}
	return old, d, nil
}
func (s *Service) Get(ctx context.Context, projectID, environmentID, key string) (evaluation.Definition, error) {
	var body []byte
	err := s.pool.QueryRow(ctx, `SELECT r.definition FROM flags f JOIN environment_flag_state s ON s.flag_id=f.id AND s.project_id=f.project_id JOIN flag_revisions r ON r.flag_id=s.flag_id AND r.environment_id=s.environment_id AND r.revision=s.current_revision
	WHERE f.project_id=$1 AND s.environment_id=$2 AND f.key=$3`, projectID, environmentID, key).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return evaluation.Definition{}, ErrNotFound
	}
	if err != nil {
		return evaluation.Definition{}, err
	}
	var d evaluation.Definition
	err = json.Unmarshal(body, &d)
	return d, err
}
func (s *Service) List(ctx context.Context, actor auth.Actor, projectID, environmentID string) ([]evaluation.Definition, error) {
	if err := auth.Authorize(ctx, s.pool, actor, projectID, environmentID, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT r.definition FROM flags f JOIN environment_flag_state s ON s.flag_id=f.id AND s.project_id=f.project_id JOIN flag_revisions r ON r.flag_id=s.flag_id AND r.environment_id=s.environment_id AND r.revision=s.current_revision WHERE f.project_id=$1 AND s.environment_id=$2 ORDER BY f.key LIMIT 100`, projectID, environmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]evaluation.Definition, 0)
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var d evaluation.Definition
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, rows.Err()
}
func saveRevision(ctx context.Context, tx pgx.Tx, actor auth.Actor, d *evaluation.Definition, c *evaluation.Compiled) error {
	body, err := json.Marshal(c)
	if err != nil {
		return err
	}
	// Validate the PostgreSQL representation too: jsonb normalizes number spelling and whitespace.
	var normalized []byte
	if err := tx.QueryRow(ctx, `SELECT $1::jsonb`, body).Scan(&normalized); err != nil {
		return auth.ErrInvalid
	}
	var canonical evaluation.Definition
	if json.Unmarshal(normalized, &canonical) != nil {
		return auth.ErrInvalid
	}
	if _, err := evaluation.Compile(canonical); err != nil {
		return auth.ErrInvalid
	}
	*d = canonical
	body = normalized
	if _, err = tx.Exec(ctx, `INSERT INTO flag_revisions(project_id,environment_id,flag_id,revision,definition,created_by) VALUES($1,$2,$3,$4,$5,$6)`, d.ProjectID, d.EnvironmentID, d.FlagID, d.Revision, body, actor.ID); err != nil {
		return err
	}
	if err = outbox.Record(ctx, tx, outbox.Reference{Kind: "configuration", ProjectID: d.ProjectID, EnvironmentID: d.EnvironmentID, ObjectID: d.FlagID, Revision: d.Revision}); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO environment_flag_state(project_id,environment_id,flag_id,current_revision) VALUES($1,$2,$3,$4) ON CONFLICT(flag_id,environment_id) DO UPDATE SET current_revision=excluded.current_revision`, d.ProjectID, d.EnvironmentID, d.FlagID, d.Revision)
	return err
}

// LockCurrent serializes configuration mutations with experiment lifecycle operations.
// The caller must authorize the actor in the same transaction before calling it.
func LockCurrent(ctx context.Context, tx pgx.Tx, projectID, environmentID, key string) (evaluation.Definition, error) {
	var flagID string
	err := tx.QueryRow(ctx, `SELECT id FROM flags WHERE project_id=$1 AND key=$2 FOR UPDATE`, projectID, key).Scan(&flagID)
	if errors.Is(err, pgx.ErrNoRows) {
		return evaluation.Definition{}, ErrNotFound
	}
	if err != nil {
		return evaluation.Definition{}, err
	}
	var body []byte
	err = tx.QueryRow(ctx, `SELECT r.definition FROM environment_flag_state s JOIN flag_revisions r ON r.flag_id=s.flag_id AND r.environment_id=s.environment_id AND r.revision=s.current_revision WHERE s.flag_id=$1 AND s.environment_id=$2`, flagID, environmentID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return evaluation.Definition{}, ErrNotFound
	}
	if err != nil {
		return evaluation.Definition{}, err
	}
	var d evaluation.Definition
	err = json.Unmarshal(body, &d)
	return d, err
}

// AppendRevision validates and persists a definition inside an already locked transaction.
// The caller owns authorization, expected-revision checks, audit and commit.
func AppendRevision(ctx context.Context, tx pgx.Tx, actor auth.Actor, d *evaluation.Definition) error {
	c, err := evaluation.Compile(*d)
	if err != nil {
		return auth.ErrInvalid
	}
	return saveRevision(ctx, tx, actor, d, c)
}
func rollback(tx pgx.Tx) { _ = tx.Rollback(context.Background()) }
func classify(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) && p.Code == "23505" {
		return auth.ErrConflict
	}
	return err
}
func normalize(c Configuration) (Configuration, error) {
	if c.Rollout != nil {
		r := *c.Rollout
		if r.Salt != "" && r.Salt != "standalone-v1" {
			return Configuration{}, auth.ErrInvalid
		}
		r.Salt = "standalone-v1"
		c.Rollout = &r
	}
	return c, nil
}
