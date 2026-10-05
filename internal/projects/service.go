package projects

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/audit"
	"switchyard/internal/auth"
	"switchyard/internal/platform/identity"
)

type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Environment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type CreatedKey struct {
	ID          string   `json:"id"`
	Token       string   `json:"token"`
	Permissions []string `json:"permissions"`
}
type Service struct {
	pool    *pgxpool.Pool
	revoked func(keyID string)
}

// SetRevocationObserver registers a function called after a key revocation commits, so caches of
// successful key lookups can forget it at once. It must be set before the service is shared.
func (s *Service) SetRevocationObserver(f func(keyID string)) { s.revoked = f }

func New(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Create(ctx context.Context, actor auth.Actor, name, requestID string) (Project, error) {
	name = strings.TrimSpace(name)
	if len(name) < 1 || len(name) > 120 {
		return Project{}, auth.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Project{}, err
	}
	defer rollback(tx)
	if err := auth.RequireRole(ctx, tx, actor, "developer", "admin"); err != nil {
		return Project{}, err
	}
	p := Project{ID: identity.New("prj_"), Name: name}
	if _, err = tx.Exec(ctx, `INSERT INTO projects(id,name) VALUES($1,$2)`, p.ID, p.Name); err != nil {
		return Project{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO project_memberships(project_id,user_id) VALUES($1,$2)`, p.ID, actor.ID); err != nil {
		return Project{}, err
	}
	for _, env := range []string{"development", "staging", "production"} {
		if _, err = tx.Exec(ctx, `INSERT INTO environments(id,project_id,name) VALUES($1,$2,$3)`, identity.New("env_"), p.ID, env); err != nil {
			return Project{}, err
		}
	}
	details, err := json.Marshal(map[string]string{"name": p.Name})
	if err != nil {
		return Project{}, err
	}
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: "human", ProjectID: p.ID, Action: "project.created", RequestID: requestID, Reason: "create project", Details: details}); err != nil {
		return Project{}, err
	}
	return p, tx.Commit(ctx)
}
func (s *Service) List(ctx context.Context, actor auth.Actor) ([]Project, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.id,p.name FROM projects p JOIN project_memberships m ON m.project_id=p.id JOIN users u ON u.id=m.user_id WHERE u.id=$1 AND u.active ORDER BY p.created_at,p.id LIMIT 100`, actor.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Project, 0)
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}
func (s *Service) Environments(ctx context.Context, actor auth.Actor, projectID string) ([]Environment, error) {
	if err := auth.Authorize(ctx, s.pool, actor, projectID, "", false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id,name FROM environments WHERE project_id=$1 ORDER BY CASE name WHEN 'development' THEN 1 WHEN 'staging' THEN 2 ELSE 3 END`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Environment, 0)
	for rows.Next() {
		var e Environment
		if err := rows.Scan(&e.ID, &e.Name); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}
func (s *Service) AddMember(ctx context.Context, actor auth.Actor, projectID, userID, requestID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := auth.RequireRole(ctx, tx, actor, "admin"); err != nil {
		return err
	}
	if err := auth.Authorize(ctx, tx, actor, projectID, "", false); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO project_memberships(project_id,user_id) VALUES($1,$2)`, projectID, userID); err != nil {
		return classify(err)
	}
	details, err := json.Marshal(map[string]string{"user_id": userID})
	if err != nil {
		return err
	}
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: "human", ProjectID: projectID, Action: "project.member_added", RequestID: requestID, Reason: "grant project membership", Details: details}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (s *Service) CreateUser(ctx context.Context, actor auth.Actor, email, password, role, requestID string) (auth.Actor, error) {
	email, err := auth.NormalizeEmail(email)
	if err != nil || !auth.ValidRole(role) {
		return auth.Actor{}, auth.ErrInvalid
	}
	// Check before spending bcrypt CPU, then re-check inside the write transaction.
	if err := auth.RequireRole(ctx, s.pool, actor, "admin"); err != nil {
		return auth.Actor{}, err
	}
	hash, err := auth.PasswordHash(password)
	if err != nil {
		return auth.Actor{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return auth.Actor{}, err
	}
	defer rollback(tx)
	if err := auth.RequireRole(ctx, tx, actor, "admin"); err != nil {
		return auth.Actor{}, err
	}
	u := auth.Actor{ID: identity.New("usr_"), Email: email, Role: role}
	if _, err = tx.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES($1,$2,$3,$4)`, u.ID, u.Email, hash, u.Role); err != nil {
		return auth.Actor{}, classify(err)
	}
	details, err := json.Marshal(map[string]string{"user_id": u.ID, "role": role})
	if err != nil {
		return auth.Actor{}, err
	}
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: "human", Action: "user.created", RequestID: requestID, Reason: "create organization user", Details: details}); err != nil {
		return auth.Actor{}, err
	}
	return u, tx.Commit(ctx)
}

// normalizePermissions accepts either a runtime key (evaluate, events, config reads) or an
// agent key (context read and proposal submit). One key cannot do both.
func normalizePermissions(permissions []string) ([]string, error) {
	permissions = slices.Clone(permissions)
	slices.Sort(permissions)
	agent, runtime := false, false
	for i, p := range permissions {
		switch p {
		case "context:read", "proposals:submit":
			agent = true
		case "evaluate", "events:write", "config:read":
			runtime = true
		default:
			return nil, auth.ErrInvalid
		}
		if i > 0 && permissions[i-1] == p {
			return nil, auth.ErrInvalid
		}
	}
	if agent && runtime {
		return nil, auth.ErrInvalid
	}
	return permissions, nil
}

func (s *Service) CreateKey(ctx context.Context, actor auth.Actor, projectID, environmentID, name string, permissions []string, requestID string) (CreatedKey, error) {
	name = strings.TrimSpace(name)
	if len(name) < 1 || len(name) > 120 || len(permissions) < 1 || len(permissions) > 3 {
		return CreatedKey{}, auth.ErrInvalid
	}
	permissions, err := normalizePermissions(permissions)
	if err != nil {
		return CreatedKey{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreatedKey{}, err
	}
	defer rollback(tx)
	agentOnly := true
	for _, p := range permissions {
		if p != "context:read" && p != "proposals:submit" {
			agentOnly = false
		}
	}
	if agentOnly {
		// A proposal key may target production because it cannot change configuration itself.
		if _, err := auth.AuthorizeEnv(ctx, tx, actor, projectID, environmentID, true); err != nil {
			return CreatedKey{}, err
		}
	} else if err := auth.Authorize(ctx, tx, actor, projectID, environmentID, true); err != nil {
		return CreatedKey{}, err
	}
	k := CreatedKey{ID: identity.New("key_"), Token: identity.New("swk_"), Permissions: permissions}
	if _, err = tx.Exec(ctx, `INSERT INTO application_keys(id,token_hash,project_id,environment_id,name,permissions,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)`, k.ID, identity.Hash(k.Token), projectID, environmentID, name, permissions, actor.ID); err != nil {
		return CreatedKey{}, err
	}
	details, err := json.Marshal(map[string]any{"key_id": k.ID, "name": name, "permissions": permissions})
	if err != nil {
		return CreatedKey{}, err
	}
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: "human", ProjectID: projectID, EnvironmentID: environmentID, Action: "application_key.created", RequestID: requestID, Reason: "create scoped application key", Details: details}); err != nil {
		return CreatedKey{}, err
	}
	return k, tx.Commit(ctx)
}
func (s *Service) RevokeKey(ctx context.Context, actor auth.Actor, projectID, keyID, requestID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	if err := auth.RequireRole(ctx, tx, actor, "developer", "admin"); err != nil {
		return err
	}
	if err := auth.Authorize(ctx, tx, actor, projectID, "", false); err != nil {
		return err
	}
	var environmentID string
	err = tx.QueryRow(ctx, `UPDATE application_keys SET revoked_at=now() WHERE id=$1 AND project_id=$2 AND revoked_at IS NULL RETURNING environment_id`, keyID, projectID).Scan(&environmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.ErrConflict
	}
	if err != nil {
		return err
	}
	details, err := json.Marshal(map[string]string{"key_id": keyID})
	if err != nil {
		return err
	}
	if err = audit.Record(ctx, tx, audit.Entry{ActorID: actor.ID, Source: "human", ProjectID: projectID, EnvironmentID: environmentID, Action: "application_key.revoked", RequestID: requestID, Reason: "revoke application credential", Details: details}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	if s.revoked != nil {
		s.revoked(keyID) // after commit: caches must never forget a revocation that rolled back
	}
	return nil
}
func rollback(tx pgx.Tx) { _ = tx.Rollback(context.Background()) }
func classify(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) {
		if p.Code == "23505" {
			return auth.ErrConflict
		}
		if p.Code == "23503" || p.Code == "23514" {
			return auth.ErrInvalid
		}
	}
	return err
}
