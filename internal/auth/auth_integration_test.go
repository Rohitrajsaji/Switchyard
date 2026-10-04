//go:build integration

package auth_test

import (
	"context"
	"errors"
	"testing"

	"switchyard/internal/auth"
	"switchyard/internal/platform/identity"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	"switchyard/migrations"
)

func TestSessionsExpiryRevocationAndScopedKeys(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	hash, err := auth.PasswordHash("a-good-demo-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test',$1,'admin')`, hash); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"missing@example.test", "admin@example.test"} {
		if _, err := a.Login(ctx, email, "incorrect-password"); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatalf("bad login: %v", err)
		}
	}
	session, err := a.Login(ctx, "ADMIN@example.test", "a-good-demo-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	current, err := a.Current(ctx, session.Token)
	if err != nil || current.CSRF != session.CSRF {
		t.Fatalf("CSRF reload mismatch: %v", err)
	}
	if err := a.VerifyCSRF(ctx, session.Token, session.CSRF); err != nil {
		t.Fatal(err)
	}
	if err := a.VerifyCSRF(ctx, session.Token, identity.New("swc_")); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("bad csrf: %v", err)
	}
	var matches bool
	if err := pool.QueryRow(ctx, `SELECT token_hash=$1 AND csrf_hash=$2 FROM sessions`, identity.Hash(session.Token), identity.Hash(session.CSRF)).Scan(&matches); err != nil || !matches {
		t.Fatalf("credentials not hashed: %v", err)
	}
	p := projects.New(pool)
	project, err := p.Create(ctx, session.Actor, "Test marketplace", "create-project")
	if err != nil {
		t.Fatal(err)
	}
	var dev, staging string
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, project.ID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='staging'`, project.ID).Scan(&staging); err != nil {
		t.Fatal(err)
	}
	key, err := p.CreateKey(ctx, session.Actor, project.ID, dev, "Go server", []string{"evaluate"}, "create-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AuthenticateApplication(ctx, key.Token, project.ID, dev, "evaluate"); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ project, env, permission string }{{project.ID, staging, "evaluate"}, {"other", dev, "evaluate"}, {project.ID, dev, "events:write"}} {
		if _, err := a.AuthenticateApplication(ctx, key.Token, tt.project, tt.env, tt.permission); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("scope bypass: %v", err)
		}
	}
	if _, err := a.Authenticate(ctx, key.Token); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("app key accepted as human")
	}
	if err := p.RevokeKey(ctx, session.Actor, project.ID, key.ID, "revoke-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AuthenticateApplication(ctx, key.Token, project.ID, dev, "evaluate"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("revoked app key accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE sessions SET expires_at=now()-interval '1 second' WHERE token_hash=$1`, identity.Hash(session.Token)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, session.Token); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("expired session accepted")
	}
	session, err = a.Login(ctx, "admin@example.test", "a-good-demo-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Logout(ctx, session.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, session.Token); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("logged-out session accepted")
	}
}
