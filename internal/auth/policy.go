package auth

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Queryer is the narrow boundary shared by pools and transactions.
type Queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Authorize re-reads the current role and membership inside the operation's transaction.
// This prevents callers forging Actor.Role or continuing after a permission change.
// Production writes are denied here by default: only the central change policy
// (flags exempt changes and approved proposals) may use AuthorizeEnv to mutate production.
func Authorize(ctx context.Context, db Queryer, actor Actor, projectID, environmentID string, write bool) error {
	name, err := AuthorizeEnv(ctx, db, actor, projectID, environmentID, write)
	if err != nil {
		return err
	}
	if write && name == "production" {
		return ErrForbidden
	}
	return nil
}

// AuthorizeEnv performs the role, membership and environment-scope checks of Authorize and
// returns the environment name without applying the production restriction. Callers must
// enforce the central production policy themselves.
func AuthorizeEnv(ctx context.Context, db Queryer, actor Actor, projectID, environmentID string, write bool) (string, error) {
	var role string
	err := db.QueryRow(ctx, `SELECT u.role FROM users u JOIN project_memberships m ON m.user_id=u.id
	WHERE u.id=$1 AND u.active AND m.project_id=$2`, actor.ID, projectID).Scan(&role)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", ErrForbidden
		}
		return "", err
	}
	if write && role == "viewer" {
		return "", ErrForbidden
	}
	var name string
	if environmentID != "" {
		err := db.QueryRow(ctx, `SELECT name FROM environments WHERE id=$1 AND project_id=$2`, environmentID, projectID).Scan(&name)
		if err != nil {
			if err == pgx.ErrNoRows {
				return "", ErrForbidden
			}
			return "", err
		}
	}
	return name, nil
}

func RequireRole(ctx context.Context, db Queryer, actor Actor, roles ...string) error {
	var role string
	err := db.QueryRow(ctx, `SELECT role FROM users WHERE id=$1 AND active`, actor.ID).Scan(&role)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ErrForbidden
		}
		return err
	}
	for _, allowed := range roles {
		if allowed == role {
			return nil
		}
	}
	return ErrForbidden
}
