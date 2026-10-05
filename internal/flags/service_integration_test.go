//go:build integration

package flags_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE kind='configuration'`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("revision publication intents=%d %v", count, err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_outbox_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'outbox fixture rejection'; END; $$; CREATE TRIGGER outbox_fixture BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION reject_outbox_fixture()`); err != nil {
		t.Fatal(err)
	}
	update.Configuration.Rollout.Salt = "standalone-v1"
	if _, err := s.Update(ctx, actor, p.ID, "new_listing", update, "outbox-failure"); err == nil {
		t.Fatal("revision committed without publication intent")
	}
	if d, err := s.Get(ctx, p.ID, dev, "new_listing"); err != nil || d.Revision != 2 {
		t.Fatal("outbox failure did not roll back revision")
	}
}

func TestStringAndNumberFlagRevisions(t *testing.T) {
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES('admin','admin@example.test','unused','admin')`); err != nil {
		t.Fatal(err)
	}
	actor := auth.Actor{ID: "admin"}
	p, err := projects.New(pool).Create(ctx, actor, "Typed", "typed-project")
	if err != nil {
		t.Fatal(err)
	}
	var dev string
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, p.ID).Scan(&dev); err != nil {
		t.Fatal(err)
	}
	s := flags.New(pool)
	text := func(v string) evaluation.Value {
		return evaluation.Value{Type: "string", Data: json.RawMessage(fmt.Sprintf("%q", v))}
	}
	created, err := s.Create(ctx, actor, p.ID, flags.CreateInput{EnvironmentID: dev, Key: "copy", Type: "string", Configuration: flags.Configuration{Default: text("control"), Safe: text("safe")}, Reason: "string copy"}, "string-flag")
	if err != nil || created.Type != "string" || string(created.Default.Data) != `"control"` {
		t.Fatalf("string flag: %+v %v", created, err)
	}
	number := func(raw string) evaluation.Value {
		return evaluation.Value{Type: "number", Data: json.RawMessage(raw)}
	}
	created, err = s.Create(ctx, actor, p.ID, flags.CreateInput{EnvironmentID: dev, Key: "timeout", Type: "number", Configuration: flags.Configuration{Default: number("1.5"), Safe: number("0")}, Reason: "number timeout"}, "number-flag")
	if err != nil || created.Type != "number" || string(created.Default.Data) != "1.5" {
		t.Fatalf("number flag: %+v %v", created, err)
	}
	if _, err := s.Create(ctx, actor, p.ID, flags.CreateInput{EnvironmentID: dev, Key: "bad_number", Type: "number", Configuration: flags.Configuration{Default: number(`"1"`), Safe: number("0")}, Reason: "quoted number"}, "bad-number"); !errors.Is(err, auth.ErrInvalid) {
		t.Fatalf("quoted number accepted: %v", err)
	}
}
