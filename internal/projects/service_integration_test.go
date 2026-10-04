//go:build integration

package projects_test

import (
	"context"
	"errors"
	"testing"

	"switchyard/internal/auth"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	"switchyard/migrations"
)

func TestProjectPermissionsAndAuditAtomicity(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", "developer", "viewer"} {
		if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES($1,$2,'unused',$1)`, role, role+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	s := projects.New(pool)
	p, err := s.Create(ctx, auth.Actor{ID: "admin"}, "Marketplace", "request-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, auth.Actor{ID: "viewer", Role: "admin"}, "forged", "request-2"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("forged viewer created project: %v", err)
	}
	if err := s.AddMember(ctx, auth.Actor{ID: "admin"}, p.ID, "viewer", "request-3"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, auth.Actor{ID: "admin"}, p.ID, "developer", "request-4"); err != nil {
		t.Fatal(err)
	}
	var dev, prod string
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, p.ID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='production'`, p.ID).Scan(&prod); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", "developer", "viewer"} {
		wantErr := role == "viewer"
		err := auth.Authorize(ctx, pool, auth.Actor{ID: role, Role: "admin"}, p.ID, dev, true)
		if (err != nil) != wantErr {
			t.Fatalf("%s dev write=%v", role, err)
		}
		if err := auth.Authorize(ctx, pool, auth.Actor{ID: role}, p.ID, prod, true); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("MVP production writable: %v", err)
		}
		if err := auth.Authorize(ctx, pool, auth.Actor{ID: role}, "another-project", dev, false); !errors.Is(err, auth.ErrForbidden) {
			t.Fatalf("cross-project access: %v", err)
		}
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_entries`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, auth.Actor{ID: "admin"}, p.ID, "missing-user", "request-failed"); err == nil {
		t.Fatal("invalid membership committed")
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_entries`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("failed write produced audit success")
	}
	for _, sql := range []string{`UPDATE audit_entries SET reason='rewrite'`, `DELETE FROM audit_entries`, `TRUNCATE audit_entries`} {
		if _, err := pool.Exec(ctx, sql); err == nil {
			t.Fatal("audit rewrite allowed")
		}
	}
	// Failure in audit insertion must roll back the project too.
	if _, err := s.Create(ctx, auth.Actor{ID: "admin"}, "audit failure", ""); err == nil {
		t.Fatal("missing audit context accepted")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE name='audit failure'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("audit failure left project: %d %v", count, err)
	}
}
