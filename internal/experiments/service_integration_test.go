//go:build integration

package experiments_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"switchyard/internal/auth"
	"switchyard/internal/experiments"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/testutil"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
)

func boolean(s string) evaluation.Value {
	return evaluation.Value{Type: "boolean", Data: json.RawMessage(s)}
}
func TestRunLifecycleFreezesPopulationAndCommitsAtomically(t *testing.T) {
	ctx := context.Background()
	pool := testutil.Database(t)
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
	p, err := ps.Create(ctx, actor, "Measurement", "project")
	if err != nil {
		t.Fatal(err)
	}
	if err = ps.AddMember(ctx, actor, p.ID, "viewer", "member"); err != nil {
		t.Fatal(err)
	}
	var env, prod string
	if err = pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, p.ID).Scan(&env); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='production'`, p.ID).Scan(&prod); err != nil {
		t.Fatal(err)
	}
	fs := flags.New(pool)
	cfg := flags.Configuration{Default: boolean("false"), Safe: boolean("false")}
	d, err := fs.Create(ctx, actor, p.ID, flags.CreateInput{EnvironmentID: env, Key: "listing", Type: "boolean", Configuration: cfg, Reason: "baseline"}, "flag")
	if err != nil {
		t.Fatal(err)
	}
	s := experiments.New(pool)
	input := experiments.CreateInput{EnvironmentID: env, FlagKey: d.Key, ExpectedRevision: 1, Name: "Simpler listing", ControlVariantID: "control", TrafficBP: 10000, Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 5000, Value: boolean("false")}, {ID: "treatment", Ordinal: 1, WeightBP: 5000, Value: boolean("true")}}, Reason: "measure completion"}
	if _, err = s.Create(ctx, auth.Actor{ID: "viewer", Role: "admin"}, p.ID, input, "bad-role"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("viewer: %v", err)
	}
	bad := input
	bad.EnvironmentID = prod
	if _, err = s.Create(ctx, actor, p.ID, bad, "prod"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("production: %v", err)
	}
	if _, err = s.Create(ctx, actor, p.ID, input, ""); err == nil {
		t.Fatal("missing audit accepted")
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM experiment_runs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed audit persisted run: %d %v", count, err)
	}
	run, err := s.Create(ctx, actor, p.ID, input, "create")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Create(ctx, actor, p.ID, input, "duplicate"); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("two reservations: %v", err)
	}
	if _, err = fs.Update(ctx, actor, p.ID, d.Key, flags.UpdateInput{EnvironmentID: env, ExpectedRevision: 1, Configuration: cfg, Reason: "change"}, "edit"); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("draft changed: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE experiment_runs SET definition=jsonb_set(definition,'{experiment,traffic_bp}','9000') WHERE id=$1`, run.ID); err == nil {
		t.Fatal("population rewrite accepted")
	}
	start := experiments.TransitionInput{ExpectedRevision: 1, Action: "start", Reason: "launch"}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := s.Transition(ctx, actor, p.ID, run.ID, start, "start"); errs <- err })
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
		t.Fatalf("start winners %d conflicts %d", success, conflict)
	}
	live, err := fs.Get(ctx, p.ID, env, d.Key)
	if err != nil || live.Revision != 2 || live.Experiment == nil {
		t.Fatalf("running definition: %+v %v", live, err)
	}
	before, _ := evaluation.Compile(live)
	if _, err = s.Transition(ctx, actor, p.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 2, Action: "pause", Reason: "inspect"}, ""); err == nil {
		t.Fatal("audit failure accepted")
	}
	still, err := fs.Get(ctx, p.ID, env, d.Key)
	if err != nil || still.Revision != 2 {
		t.Fatal("audit failure changed config")
	}
	paused, err := s.Transition(ctx, actor, p.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 2, Action: "pause", Reason: "inspect"}, "pause")
	if err != nil || paused.State != "paused" {
		t.Fatalf("pause %v", err)
	}
	if _, err = fs.Update(ctx, actor, p.ID, d.Key, flags.UpdateInput{EnvironmentID: env, ExpectedRevision: 3, Configuration: cfg, Reason: "change"}, "edit"); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("paused population changed")
	}
	if _, err = s.Transition(ctx, actor, p.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 3, Action: "start", Reason: "resume"}, "resume"); err != nil {
		t.Fatal(err)
	}
	resumed, err := fs.Get(ctx, p.ID, env, d.Key)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := evaluation.Compile(resumed)
	for _, user := range []string{"a", "b", "c", "d"} {
		x, _ := before.Evaluate(user, nil)
		y, _ := after.Evaluate(user, nil)
		if x.VariantID != y.VariantID || x.RunID != y.RunID {
			t.Fatal("resume reshuffled assignment")
		}
	}
	completed, err := s.Transition(ctx, actor, p.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 4, Action: "complete", Reason: "finished"}, "complete")
	if err != nil || completed.CompletedAt == nil || completed.StartedAt == nil {
		t.Fatalf("complete: %+v %v", completed, err)
	}
	if completed.Definition.Revision != 1 || completed.Definition.Experiment.RunID != run.ID {
		t.Fatal("historical definition changed")
	}
	if _, err = s.Transition(ctx, actor, p.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 5, Action: "start", Reason: "restart"}, "restart"); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("completed run restarted")
	}
	if _, err = fs.Update(ctx, actor, p.ID, d.Key, flags.UpdateInput{EnvironmentID: env, ExpectedRevision: 5, Configuration: cfg, Reason: "next experiment"}, "edit"); err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision = 6
	next, err := s.Create(ctx, actor, p.ID, input, "new-run")
	if err != nil || next.ID == run.ID {
		t.Fatalf("new run: %v", err)
	}
	if _, err = s.Get(ctx, actor, "other-project", run.ID); !errors.Is(err, auth.ErrForbidden) {
		t.Fatalf("cross-project: %v", err)
	}
	// Safety disable remains available even while a run reserves its population.
	if _, err = s.Transition(ctx, actor, p.ID, next.ID, experiments.TransitionInput{ExpectedRevision: 6, Action: "start", Reason: "launch second run"}, "start-second"); err != nil {
		t.Fatal(err)
	}
	kill := cfg
	kill.Killed = true
	unsafe := kill
	unsafe.Default = boolean("true")
	if _, err = fs.Update(ctx, actor, p.ID, d.Key, flags.UpdateInput{EnvironmentID: env, ExpectedRevision: 7, Configuration: unsafe, Reason: "disguised edit"}, "unsafe"); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("kill hid config edit: %v", err)
	}
	killed, err := fs.Update(ctx, actor, p.ID, d.Key, flags.UpdateInput{EnvironmentID: env, ExpectedRevision: 7, Configuration: kill, Reason: "emergency disable"}, "kill")
	if err != nil || !killed.Killed {
		t.Fatalf("reserved kill: %v", err)
	}
	compiled, err := evaluation.Compile(killed)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := compiled.Evaluate("a", nil)
	if err != nil || decision.Reason != "kill_switch" || !evaluation.Equal(decision.Value, cfg.Safe) {
		t.Fatalf("active kill failed: %+v %v", decision, err)
	}
	if _, err = s.Transition(ctx, actor, p.ID, next.ID, experiments.TransitionInput{ExpectedRevision: 8, Action: "pause", Reason: "pause after kill"}, "pause-killed"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Transition(ctx, actor, p.ID, next.ID, experiments.TransitionInput{ExpectedRevision: 9, Action: "start", Reason: "unsafe restart"}, "restart"); !errors.Is(err, auth.ErrConflict) {
		t.Fatalf("killed run started: %v", err)
	}
}
