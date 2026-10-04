//go:build integration

package flags_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"switchyard/internal/auth"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
)

func value(v string) evaluation.Value {
	return evaluation.Value{Type: "boolean", Data: json.RawMessage(v)}
}
func TestFlagRevisionConflictScopeAndAtomicity(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"admin", "viewer"} {
		if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES($1,$2,'unused',$1)`, role, role+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	actor := auth.Actor{ID: "admin"}
	ps := projects.New(pool)
	p, err := ps.Create(ctx, actor, "Flags", "project-request")
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.AddMember(ctx, actor, p.ID, "viewer", "member-request"); err != nil {
		t.Fatal(err)
	}
	var dev, prod string
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, p.ID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='production'`, p.ID).Scan(&prod); err != nil {
		t.Fatal(err)
	}
	s := flags.New(pool)
	input := flags.CreateInput{EnvironmentID: dev, Key: "new_listing", Type: "boolean", Configuration: flags.Configuration{Default: value("false"), Safe: value("false"), Rollout: &evaluation.Rollout{TrafficBP: 1000, Value: value("true")}}, Reason: "demo listing flow"}
	d, err := s.Create(ctx, actor, p.ID, input, "flag-create")
	if err != nil {
		t.Fatal(err)
	}
	if d.Revision != 1 || d.Rollout.Salt != "standalone-v1" {
		t.Fatal("missing canonical first revision/salt")
	}
	input.Key = "viewer_flag"
	if _, err := s.Create(ctx, auth.Actor{ID: "viewer", Role: "admin"}, p.ID, input, "bad-role"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("viewer wrote flag")
	}
	input.EnvironmentID = prod
	if _, err := s.Create(ctx, actor, p.ID, input, "bad-prod"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("production wrote flag")
	}
	update := flags.UpdateInput{EnvironmentID: dev, ExpectedRevision: 1, Configuration: flags.Configuration{Default: value("false"), Safe: value("false"), Rollout: &evaluation.Rollout{TrafficBP: 2000, Value: value("true")}}, Reason: "increase development traffic"}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := s.Update(ctx, actor, p.ID, "new_listing", update, "flag-update"); errs <- err })
	}
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else if errors.Is(err, auth.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	d, err = s.Get(ctx, p.ID, dev, "new_listing")
	if err != nil || d.Revision != 2 {
		t.Fatalf("revision=%d err=%v", d.Revision, err)
	}
	update.ExpectedRevision = 2
	if _, err := s.Update(ctx, actor, p.ID, "new_listing", update, ""); err == nil {
		t.Fatal("audit context missing accepted")
	}
	d, err = s.Get(ctx, p.ID, dev, "new_listing")
	if err != nil || d.Revision != 2 {
		t.Fatal("failed audit changed revision")
	}
	update.Configuration.Rollout.Salt = "new-population"
	if _, err := s.Update(ctx, actor, p.ID, "new_listing", update, "bad-salt"); !errors.Is(err, auth.ErrInvalid) {
		t.Fatal("rollout salt changed")
	}
	if _, err := s.Get(ctx, "other-project", dev, "new_listing"); !errors.Is(err, flags.ErrNotFound) {
		t.Fatal("cross-project flag leaked")
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM flag_revisions`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("history=%d err=%v", count, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE flag_revisions SET revision=99`); err == nil {
		t.Fatal("history mutable")
	}
}
