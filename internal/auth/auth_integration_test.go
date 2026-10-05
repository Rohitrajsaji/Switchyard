//go:build integration

package auth_test

import (
	"context"
	"errors"
	"golang.org/x/crypto/bcrypt"
	"strings"
	"testing"
	"time"

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
	if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost != 12 {
		t.Fatal("Production password cost changed", cost, err)
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
	p.SetRevocationObserver(a.ForgetApplication) // the composition root wires revocation to the cache
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

// Evaluation authentication remembers a successful key lookup for a bounded time. A revocation
// processed elsewhere is honored once that time passes; a revocation processed by this instance,
// and every event-ingestion check, take effect immediately.
func TestApplicationKeyCacheBoundsRemoteRevocationAndLeavesIngestionAuthoritative(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	admin := auth.Actor{ID: "admin"}
	a, err := auth.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	a.UseClock(func() time.Time { return now })
	p := projects.New(pool)
	p.SetRevocationObserver(a.ForgetApplication)
	project, err := p.Create(ctx, admin, "Cache", "project")
	if err != nil {
		t.Fatal(err)
	}
	var dev, staging string
	for name, dst := range map[string]*string{"development": &dev, "staging": &staging} {
		if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name=$2`, project.ID, name).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	remote, err := p.CreateKey(ctx, admin, project.ID, dev, "remote", []string{"evaluate", "events:write"}, "k1")
	if err != nil {
		t.Fatal(err)
	}
	local, err := p.CreateKey(ctx, admin, project.ID, dev, "local", []string{"evaluate"}, "k2")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{remote.Token, local.Token} {
		if _, err := a.AuthenticateApplication(ctx, key, project.ID, dev, "evaluate"); err != nil {
			t.Fatal(err)
		}
	}
	// Scope and permission are still enforced when the lookup is served from memory.
	if _, err := a.AuthenticateApplication(ctx, remote.Token, project.ID, staging, "evaluate"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("cached key crossed environments", err)
	}
	if _, err := a.AuthenticateApplication(ctx, local.Token, project.ID, dev, "events:write"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("cached key gained a permission", err)
	}
	// Revocation by another instance (direct database write): authenticated until the TTL, then denied.
	if _, err := pool.Exec(ctx, `UPDATE application_keys SET revoked_at=now() WHERE id=$1`, remote.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(auth.ApplicationCacheTTL - time.Nanosecond)
	if _, err := a.AuthenticateApplication(ctx, remote.Token, project.ID, dev, "evaluate"); err != nil {
		t.Fatal("cache expired early", err)
	}
	// Event ingestion consults the database inside its transaction and is never delayed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := auth.AuthorizeApplication(ctx, tx, remote.Token, project.ID, dev, "events:write"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("ingestion honored a revoked key from memory", err)
	}
	now = now.Add(time.Nanosecond)
	if _, err := a.AuthenticateApplication(ctx, remote.Token, project.ID, dev, "evaluate"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("revoked key survived the TTL", err)
	}
	// Revocation processed by this instance is immediate, not bounded by the TTL.
	if _, err := a.AuthenticateApplication(ctx, local.Token, project.ID, dev, "evaluate"); err != nil {
		t.Fatal(err)
	}
	if err := p.RevokeKey(ctx, admin, project.ID, local.ID, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AuthenticateApplication(ctx, local.Token, project.ID, dev, "evaluate"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Fatal("local revocation was not immediate", err)
	}
	// Unknown tokens are never remembered.
	unknown := "swk_" + strings.Repeat("a", 64)
	for range 2 {
		if _, err := a.AuthenticateApplication(ctx, unknown, project.ID, dev, "evaluate"); !errors.Is(err, auth.ErrUnauthorized) {
			t.Fatal("unknown token accepted", err)
		}
	}
}
