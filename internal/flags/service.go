package flags

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/audit"
	"switchyard/internal/auth"
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
func (s *Service) Create(ctx context.Context, actor auth.Actor, projectID string, in CreateInput, requestID string) (evaluation.Definition, error) {
	configuration, err := normalize(in.Configuration)
	if err != nil {
		return evaluation.Definition{}, err
	}
	in.Configuration = configuration
	if len(in.Reason) < 1 || len(in.Reason) > 512 {
		return evaluation.Definition{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return evaluation.Definition{}, err
	}
	defer rollback(tx)
	if err := auth.Authorize(ctx, tx, actor, projectID, in.EnvironmentID, true); err != nil {
		return evaluation.Definition{}, err
	}
	d := definition(projectID, in.EnvironmentID, identity.New("flg_"), in.Key, in.Type, 1, in.Configuration)
	c, err := evaluation.Compile(d)
	if err != nil {
		return evaluation.Definition{}, auth.ErrInvalid
	}
	if _, err = tx.Exec(ctx, `INSERT INTO flags(id,project_id,key,type) VALUES($1,$2,$3,$4)`, d.FlagID, projectID, d.Key, d.Type); err != nil {
		return evaluation.Definition{}, classify(err)
	}
	if err = saveRevision(ctx, tx, actor, &d, c); err != nil {
		return evaluation.Definition{}, err
	}
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: "human", ProjectID: projectID, EnvironmentID: in.EnvironmentID, Action: "flag.created", RequestID: requestID, Reason: in.Reason, AfterRevision: &d.Revision, Details: keyDetails(d.Key)}); err != nil {
		return evaluation.Definition{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return evaluation.Definition{}, err
	}
	return d, nil
}
func (s *Service) Update(ctx context.Context, actor auth.Actor, projectID, key string, in UpdateInput, requestID string) (evaluation.Definition, error) {
	configuration, err := normalize(in.Configuration)
	if err != nil {
		return evaluation.Definition{}, err
	}
	in.Configuration = configuration
	if in.ExpectedRevision < 0 || len(in.Reason) < 1 || len(in.Reason) > 512 {
		return evaluation.Definition{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return evaluation.Definition{}, err
	}
	defer rollback(tx)
	if err := auth.Authorize(ctx, tx, actor, projectID, in.EnvironmentID, true); err != nil {
		return evaluation.Definition{}, err
	}
	var flagID, kind string
	// One flag-level row lock also serializes first configuration in a new environment.
	err = tx.QueryRow(ctx, `SELECT id,type FROM flags WHERE project_id=$1 AND key=$2 FOR UPDATE`, projectID, key).Scan(&flagID, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return evaluation.Definition{}, ErrNotFound
	}
	if err != nil {
		return evaluation.Definition{}, err
	}
	var before int64
	err = tx.QueryRow(ctx, `SELECT current_revision FROM environment_flag_state WHERE flag_id=$1 AND environment_id=$2`, flagID, in.EnvironmentID).Scan(&before)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return evaluation.Definition{}, err
	}
	if before != in.ExpectedRevision {
		return evaluation.Definition{}, auth.ErrConflict
	}
	var reserved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM experiment_runs WHERE flag_id=$1 AND environment_id=$2 AND state<>'completed')`, flagID, in.EnvironmentID).Scan(&reserved); err != nil {
		return evaluation.Definition{}, err
	}
	d := definition(projectID, in.EnvironmentID, flagID, key, kind, before+1, in.Configuration)
	if before > 0 {
		var body []byte
		if err := tx.QueryRow(ctx, `SELECT definition FROM flag_revisions WHERE flag_id=$1 AND environment_id=$2 AND revision=$3`, flagID, in.EnvironmentID, before).Scan(&body); err != nil {
			return evaluation.Definition{}, err
		}
		var old evaluation.Definition
		if err := json.Unmarshal(body, &old); err != nil {
			return evaluation.Definition{}, err
		}
		if reserved || old.Experiment != nil {
			// A frozen population permits only an emergency disable, with every
			// other field unchanged. Preserve attached run metadata for history.
			if old.Killed || !d.Killed {
				return evaluation.Definition{}, auth.ErrConflict
			}
			d.Experiment = old.Experiment
			candidate := d
			candidate.Revision, candidate.Killed = old.Revision, old.Killed
			compiled, err := evaluation.Compile(candidate)
			if err != nil {
				return evaluation.Definition{}, auth.ErrInvalid
			}
			candidateBody, err := json.Marshal(compiled)
			if err != nil {
				return evaluation.Definition{}, err
			}
			// Compare complete configurations as jsonb, not as a single flag
			// value: the definition may legitimately exceed a value's 16 KiB cap.
			var unchanged bool
			if err := tx.QueryRow(ctx, `SELECT $1::jsonb = $2::jsonb`, candidateBody, body).Scan(&unchanged); err != nil {
				return evaluation.Definition{}, err
			}
			if !unchanged {
				return evaluation.Definition{}, auth.ErrConflict
			}
		}
		// Salt changes would reshuffle users; changing the population requires a new flag/run.
		if old.Rollout != nil && d.Rollout != nil && old.Rollout.Salt != d.Rollout.Salt {
			return evaluation.Definition{}, auth.ErrInvalid
		}
	}
	c, err := evaluation.Compile(d)
	if err != nil {
		return evaluation.Definition{}, auth.ErrInvalid
	}
	if err = saveRevision(ctx, tx, actor, &d, c); err != nil {
		return evaluation.Definition{}, err
	}
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: "human", ProjectID: projectID, EnvironmentID: in.EnvironmentID, Action: "flag.updated", RequestID: requestID, Reason: in.Reason, BeforeRevision: &before, AfterRevision: &d.Revision, Details: keyDetails(key)}); err != nil {
		return evaluation.Definition{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return evaluation.Definition{}, err
	}
	return d, nil
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
func keyDetails(key string) json.RawMessage {
	b, err := json.Marshal(map[string]string{"flag_key": key})
	if err != nil {
		panic("string JSON encoding failed")
	}
	return b
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
