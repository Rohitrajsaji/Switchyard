//go:build integration

package rollouts_test

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/auth"
	"switchyard/internal/experiments"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/rollouts"
	"switchyard/internal/testutil"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
)

var (
	admin1    = auth.Actor{ID: "admin1"}
	admin2    = auth.Actor{ID: "admin2"}
	developer = auth.Actor{ID: "developer"}
	viewer    = auth.Actor{ID: "viewer"}
)

func value(v string) evaluation.Value {
	return evaluation.Value{Type: "boolean", Data: json.RawMessage(v)}
}

type fixture struct {
	pool          *pgxpool.Pool
	ctx           context.Context
	project, env  string
	runID         string
	revision      int64
	clock         *atomic.Int64
	rs            *rollouts.Service
	fs            *flags.Service
	keyID         string
	eventSequence int
}

func setup(t *testing.T, traffic int) *fixture {
	t.Helper()
	pool := testutil.Database(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool, migrations.Files); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct{ id, role string }{{"admin1", "admin"}, {"admin2", "admin"}, {"developer", "developer"}, {"viewer", "viewer"}} {
		if _, err := pool.Exec(ctx, `INSERT INTO users(id,email,password_hash,role) VALUES($1,$2,'unused',$3)`, u.id, u.id+"@example.test", u.role); err != nil {
			t.Fatal(err)
		}
	}
	ps := projects.New(pool)
	p, err := ps.Create(ctx, admin1, "Rollouts", "project-request")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"admin2", "developer", "viewer"} {
		if err := ps.AddMember(ctx, admin1, p.ID, id, "member-"+id); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{pool: pool, ctx: ctx, project: p.ID, clock: &atomic.Int64{}, fs: flags.New(pool)}
	f.clock.Store(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC).UnixNano())
	f.rs = rollouts.New(pool, f.now)
	if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name='development'`, p.ID).Scan(&f.env); err != nil {
		t.Fatal(err)
	}
	cfg := flags.Configuration{Default: value("false"), Safe: value("false")}
	if _, err := f.fs.Create(ctx, admin1, p.ID, flags.CreateInput{EnvironmentID: f.env, Key: "listing", Type: "boolean", Configuration: cfg, Reason: "base"}, "r"); err != nil {
		t.Fatal(err)
	}
	es := experiments.New(pool)
	run, err := es.Create(ctx, admin1, p.ID, experiments.CreateInput{EnvironmentID: f.env, FlagKey: "listing", ExpectedRevision: 1, Name: "Listing", ControlVariantID: "control", TrafficBP: traffic,
		Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 5000, Value: value("false")}, {ID: "treatment", Ordinal: 1, WeightBP: 5000, Value: value("true")}}, Reason: "run"}, "r")
	if err != nil {
		t.Fatal(err)
	}
	started, err := es.Transition(ctx, admin1, p.ID, run.ID, experiments.TransitionInput{ExpectedRevision: 1, Action: "start", Reason: "go"}, "r")
	if err != nil {
		t.Fatal(err)
	}
	f.runID, f.revision = run.ID, started.ConfigurationRevision
	if _, err := pool.Exec(ctx, `INSERT INTO application_keys(id,token_hash,project_id,environment_id,name,permissions,created_by) VALUES('key-1','\x0102',$1,$2,'k',ARRAY['events:write'],'admin1')`, p.ID, f.env); err != nil {
		t.Fatal(err)
	}
	f.keyID = "key-1"
	return f
}
func (f *fixture) now() time.Time          { return time.Unix(0, f.clock.Load()).UTC() }
func (f *fixture) advance(d time.Duration) { f.clock.Add(int64(d)) }
func (f *fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// requests inserts accepted request-outcome facts for a variant at the injected clock's time.
func (f *fixture) requests(t *testing.T, variant string, n, errorCount int, latency float64) {
	t.Helper()
	f.eventSequence++
	prefix := variant + "-" + strconv.Itoa(f.eventSequence)
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO raw_events(project_id,environment_id,event_id,run_id,user_id,kind,variant_id,revision,exposure_id,occurred_at,received_at,status,quarantine_reason,is_error,latency_ms,payload,application_key_id)
	SELECT $1,$2,$3||'-'||g,$4,'user-'||g,'request_outcome',$5,$6,'exposure-'||g,$7,$7,'accepted','',g<=$8,$9,'{}',$10 FROM generate_series(1,$11) g`,
		f.project, f.env, prefix, f.runID, variant, f.revision, f.now(), errorCount, latency, f.keyID, n); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) plan(t *testing.T, actor auth.Actor, mode string, ceiling int, steps []rollouts.StepInput, g *rollouts.Guardrails) (rollouts.Plan, error) {
	t.Helper()
	return f.rs.Create(f.ctx, actor, f.project, rollouts.CreateInput{EnvironmentID: f.env, RunID: f.runID, Mode: mode, CeilingBP: ceiling, Steps: steps, Guardrails: g, Rationale: "ramp listing"}, "req-plan")
}
func (f *fixture) startPlan(t *testing.T, mode string, ceiling int, steps []rollouts.StepInput, g *rollouts.Guardrails) rollouts.Plan {
	t.Helper()
	p, err := f.plan(t, developer, mode, ceiling, steps, g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.rs.Approve(f.ctx, admin2, f.project, p.ID, p.PlanHash, "reviewed", "r"); err != nil {
		t.Fatal(err)
	}
	started, err := f.rs.Start(f.ctx, developer, f.project, p.ID, "r")
	if err != nil || started.State != "running" || started.Steps[0].DueAt == nil {
		t.Fatal("start", started, err)
	}
	return started
}
func (f *fixture) traffic(t *testing.T) (int, int64, bool) {
	t.Helper()
	d, err := f.fs.Get(f.ctx, f.project, f.env, "listing")
	if err != nil {
		t.Fatal(err)
	}
	if d.Experiment == nil {
		return -1, d.Revision, d.Killed
	}
	return d.Experiment.TrafficBP, d.Revision, d.Killed
}
func (f *fixture) tick(t *testing.T, id string) string {
	t.Helper()
	out, err := f.rs.Tick(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var smallGuardrails = rollouts.Guardrails{WindowSeconds: 300, MinRequests: 20, MaxErrorRate: 0.02, MaxP95MS: 500, ConsecutiveBreaches: 2, CooldownSeconds: 3600, MaxCheckAgeSeconds: 30}

func TestPlanValidationApprovalAndSeparationOfDuties(t *testing.T) {
	f := setup(t, 1000)
	ok := []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 60}}
	if _, err := f.plan(t, viewer, "scheduled", 2000, ok, nil); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("viewer created plan", err)
	}
	bad := map[string][]rollouts.StepInput{
		"too-large-increase": {{TrafficBP: 2100, OffsetSeconds: 60}},
		"not-increasing":     {{TrafficBP: 1000, OffsetSeconds: 60}},
		"above-ceiling":      {{TrafficBP: 2000, OffsetSeconds: 60}, {TrafficBP: 3000, OffsetSeconds: 120}},
		"offset-not-rising":  {{TrafficBP: 2000, OffsetSeconds: 60}, {TrafficBP: 2500, OffsetSeconds: 60}},
		"none":               {},
	}
	for name, steps := range bad {
		if _, err := f.plan(t, developer, "scheduled", 2000, steps, nil); !errors.Is(err, auth.ErrInvalid) {
			t.Fatal("invalid plan accepted:", name, err)
		}
	}
	if _, err := f.plan(t, developer, "scheduled", 2000, ok, &rollouts.Guardrails{}); !errors.Is(err, auth.ErrInvalid) {
		t.Fatal("empty guardrails accepted")
	}
	p, err := f.plan(t, developer, "scheduled", 2000, ok, nil)
	if err != nil || p.State != "proposed" || p.BaseRevision != f.revision || p.Guardrails.MinRequests != 1000 {
		t.Fatal("create", p, err)
	}
	if _, err := f.plan(t, developer, "scheduled", 2000, ok, nil); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("second live plan for one run accepted", err)
	}
	if _, err := f.rs.Start(f.ctx, developer, f.project, p.ID, "r"); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("unapproved plan started", err)
	}
	if _, err := f.rs.Approve(f.ctx, developer, f.project, p.ID, p.PlanHash, "dev", "r"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("developer approved", err)
	}
	if _, err := f.rs.Approve(f.ctx, admin2, f.project, p.ID, "0000000000000000000000000000000000000000000000000000000000000000", "x", "r"); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("wrong plan hash approved", err)
	}
	if _, err := f.rs.Cancel(f.ctx, developer, f.project, p.ID, "replace", "r"); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("cancel of a merely proposed plan should use reject", err)
	}
	if _, err := f.rs.Reject(f.ctx, developer, f.project, p.ID, "withdrawn", "r"); err != nil {
		t.Fatal(err)
	}
	// An admin cannot approve their own plan.
	own, err := f.plan(t, admin2, "scheduled", 2000, ok, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.rs.Approve(f.ctx, admin2, f.project, own.ID, own.PlanHash, "self", "r"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("proposer approved own plan", err)
	}
	// Expiry: an approved plan cannot be started after 24 hours.
	if _, err := f.rs.Approve(f.ctx, admin1, f.project, own.ID, own.PlanHash, "ok", "r"); err != nil {
		t.Fatal(err)
	}
	f.advance(rollouts.ApprovalTTL)
	if _, err := f.rs.Start(f.ctx, admin2, f.project, own.ID, "r"); !errors.Is(err, rollouts.ErrExpired) {
		t.Fatal("expired approval started", err)
	}
	if tr, rev, _ := f.traffic(t); tr != 1000 || rev != f.revision {
		t.Fatal("expired plan changed traffic")
	}
}

func TestScheduledProgressionIsBoundedIdempotentAndConcurrencySafe(t *testing.T) {
	f := setup(t, 1000)
	p := f.startPlan(t, "scheduled", 3000, []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 60}, {TrafficBP: 3000, OffsetSeconds: 120}}, nil)
	if out := f.tick(t, p.ID); out != "waiting_schedule" {
		t.Fatal("step ran before due", out)
	}
	f.advance(60 * time.Second)
	var wg sync.WaitGroup
	outcomes := make(chan string, 8)
	for range 8 {
		wg.Go(func() {
			out, err := f.rs.Tick(f.ctx, p.ID)
			if err != nil {
				t.Error(err)
			}
			outcomes <- out
		})
	}
	wg.Wait()
	close(outcomes)
	applied := 0
	for out := range outcomes {
		if out == "step_applied" {
			applied++
		}
	}
	tr, rev, killed := f.traffic(t)
	if applied != 1 || tr != 2000 || rev != f.revision+1 || killed {
		t.Fatal("duplicate or concurrent ticks repeated a step", applied, tr, rev)
	}
	if out := f.tick(t, p.ID); out != "waiting_schedule" {
		t.Fatal("applied step repeated", out)
	}
	f.advance(60 * time.Second)
	if out := f.tick(t, p.ID); out != "completed" {
		t.Fatal("final step", out)
	}
	tr, rev, _ = f.traffic(t)
	got, _ := f.rs.Get(f.ctx, admin1, f.project, p.ID)
	if tr != 3000 || rev != f.revision+2 || got.State != "completed" || got.Steps[0].State != "applied" || got.Steps[1].State != "applied" {
		t.Fatal("completion", tr, rev, got)
	}
	if f.count(t, `SELECT count(*) FROM audit_entries WHERE action='flag.traffic_changed' AND details->>'plan_id'=$1 AND actor_id='system:rollout' AND details->>'approver_id'='admin2'`, p.ID) != 2 {
		t.Fatal("step audit lacks plan evidence")
	}
	if out := f.tick(t, p.ID); out != "inactive" {
		t.Fatal("finished plan ticked", out)
	}
	// Assignments are stable: the run definition's salts and weights never changed.
	d, _ := f.fs.Get(f.ctx, f.project, f.env, "listing")
	if d.Experiment.EligibilitySalt == "" || len(d.Experiment.Variants) != 2 || d.Experiment.Variants[1].WeightBP != 5000 {
		t.Fatal("assignment definition changed", d.Experiment)
	}
}

func TestOutsideChangesStopTheScheduleAndNeverRepeatSteps(t *testing.T) {
	f := setup(t, 1000)
	steps := []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 60}, {TrafficBP: 3000, OffsetSeconds: 120}}
	// A manual pause moves the revision: the plan becomes stale and the flag is untouched.
	p := f.startPlan(t, "scheduled", 3000, steps, nil)
	if _, err := experiments.New(f.pool).Transition(f.ctx, admin1, f.project, f.runID, experiments.TransitionInput{ExpectedRevision: f.revision, Action: "pause", Reason: "manual"}, "r"); err != nil {
		t.Fatal(err)
	}
	f.advance(2 * time.Minute)
	if out := f.tick(t, p.ID); out != "stale" {
		t.Fatal("pause did not stale the plan", out)
	}
	if tr, _, _ := f.traffic(t); tr != -1 {
		t.Fatal("stale plan changed a paused run")
	}
	got, _ := f.rs.Get(f.ctx, admin1, f.project, p.ID)
	if got.State != "stale" || got.Steps[0].State != "cancelled" {
		t.Fatal("stale plan retained pending steps", got)
	}
}

func TestEmergencyKillCancelsScheduleAndNoSuccessorEnables(t *testing.T) {
	for _, order := range []string{"kill-first", "progress-first"} {
		t.Run(order, func(t *testing.T) {
			f := setup(t, 1000)
			p := f.startPlan(t, "scheduled", 3000, []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 60}, {TrafficBP: 3000, OffsetSeconds: 120}}, nil)
			f.advance(60 * time.Second)
			kill := func() {
				cfg := flags.Configuration{Default: value("false"), Safe: value("false"), Killed: true}
				_, rev, _ := f.traffic(t)
				if _, err := f.fs.Update(f.ctx, admin1, f.project, "listing", flags.UpdateInput{EnvironmentID: f.env, ExpectedRevision: rev, Configuration: cfg, Reason: "emergency"}, "r"); err != nil {
					t.Fatal(err)
				}
			}
			if order == "kill-first" {
				kill()
				if out := f.tick(t, p.ID); out != "cancelled" {
					t.Fatal("progression after kill", out)
				}
			} else {
				if out := f.tick(t, p.ID); out != "step_applied" {
					t.Fatal(out)
				}
				kill()
				f.advance(60 * time.Second)
				if out := f.tick(t, p.ID); out != "cancelled" {
					t.Fatal("progression after kill", out)
				}
			}
			_, _, killed := f.traffic(t)
			got, _ := f.rs.Get(f.ctx, admin1, f.project, p.ID)
			if !killed || got.State != "cancelled" || got.Steps[1].State != "cancelled" {
				t.Fatal("kill not honored", killed, got)
			}
		})
	}
}

func TestConcurrentKillAndProgressionNeverLeaveAnEnabledSuccessor(t *testing.T) {
	for i := 0; i < 12; i++ {
		f := setup(t, 1000)
		p := f.startPlan(t, "scheduled", 3000, []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 60}, {TrafficBP: 3000, OffsetSeconds: 61}}, nil)
		f.advance(2 * time.Minute)
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = f.rs.Tick(f.ctx, p.ID) })
		wg.Go(func() {
			cfg := flags.Configuration{Default: value("false"), Safe: value("false"), Killed: true}
			// The kill may lose a revision race against a step; retry from the new revision.
			for range 4 {
				_, rev, _ := f.traffic(t)
				if _, err := f.fs.Update(f.ctx, admin1, f.project, "listing", flags.UpdateInput{EnvironmentID: f.env, ExpectedRevision: rev, Configuration: cfg, Reason: "emergency"}, "r"); err == nil {
					return
				}
			}
			t.Error("kill could not be applied")
		})
		wg.Wait()
		for range 3 {
			f.tick(t, p.ID)
		}
		_, _, killed := f.traffic(t)
		got, _ := f.rs.Get(f.ctx, admin1, f.project, p.ID)
		if !killed || got.State == "running" {
			t.Fatal("kill lost against progression", i, killed, got.State)
		}
		// Whatever was applied before the kill, no revision after the first kill re-enables the flag.
		if f.count(t, `SELECT count(*) FROM flag_revisions WHERE environment_id=$1 AND revision>(SELECT min(revision) FROM flag_revisions WHERE environment_id=$1 AND (definition->>'killed')::boolean) AND NOT (definition->>'killed')::boolean`, f.env) != 0 {
			t.Fatal("a revision after the kill re-enabled the flag", i)
		}
	}
}

func TestMetricDrivenProgressionNeedsSufficientHealthyFreshEvidence(t *testing.T) {
	f := setup(t, 1000)
	g := smallGuardrails
	p := f.startPlan(t, "metric", 2000, []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 10}}, &g)
	f.advance(10 * time.Second)
	if out := f.tick(t, p.ID); out != "waiting_metrics" {
		t.Fatal("promoted without evidence", out)
	}
	f.requests(t, "treatment", 19, 0, 120) // one short of the minimum
	if out := f.tick(t, p.ID); out != "waiting_metrics" {
		t.Fatal("promoted on insufficient data", out)
	}
	if tr, _, _ := f.traffic(t); tr != 1000 {
		t.Fatal("traffic changed without sufficient evidence")
	}
	f.requests(t, "treatment", 1, 0, 120)
	if out := f.tick(t, p.ID); out != "completed" {
		t.Fatal("healthy sufficient evidence did not promote", out)
	}
	if tr, _, _ := f.traffic(t); tr != 2000 {
		t.Fatal("promotion not applied")
	}
	checks, err := f.rs.Checks(f.ctx, admin1, f.project, p.ID)
	if err != nil || len(checks) != 3 || checks[0].Decision != "pass" || checks[1].Decision != "insufficient" {
		t.Fatal("checks", checks, err)
	}
}

func TestStaleEvidenceAgesOutOfTheWindow(t *testing.T) {
	f := setup(t, 1000)
	g := smallGuardrails
	p := f.startPlan(t, "metric", 2000, []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 10}}, &g)
	f.requests(t, "treatment", 50, 0, 100)
	f.advance(time.Duration(g.WindowSeconds)*time.Second + time.Second) // evidence is now older than the window
	if out := f.tick(t, p.ID); out != "waiting_metrics" {
		t.Fatal("stale metrics promoted a rollout", out)
	}
	if tr, _, _ := f.traffic(t); tr != 1000 {
		t.Fatal("stale evidence changed traffic")
	}
}

func TestOperationalBreachRollsBackAfterConsecutiveChecksAndRecordsEvidence(t *testing.T) {
	f := setup(t, 1000)
	g := smallGuardrails
	p := f.startPlan(t, "scheduled", 3000, []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 600}, {TrafficBP: 3000, OffsetSeconds: 1200}}, &g)
	// Healthy control and a broken treatment: 30% errors, slow.
	f.requests(t, "control", 100, 0, 100)
	f.requests(t, "treatment", 100, 30, 900)
	before := f.revision
	if out := f.tick(t, p.ID); out != "breach_observed" {
		t.Fatal("first breach acted alone", out)
	}
	if _, rev, killed := f.traffic(t); killed || rev != before {
		t.Fatal("single breach changed configuration")
	}
	f.advance(10 * time.Second)
	if out := f.tick(t, p.ID); out != "rolled_back" {
		t.Fatal("second consecutive breach did not roll back", out)
	}
	_, rev, killed := f.traffic(t)
	got, _ := f.rs.Get(f.ctx, admin1, f.project, p.ID)
	if !killed || rev != before+1 || got.State != "rolled_back" || got.Steps[0].State != "cancelled" || got.Steps[1].State != "cancelled" {
		t.Fatal("rollback state", killed, rev, got)
	}
	var cooldown time.Time
	var evidence []byte
	var revAfter int64
	if err := f.pool.QueryRow(f.ctx, `SELECT cooldown_until,evidence,revision_after FROM safety_rollbacks WHERE plan_id=$1`, p.ID).Scan(&cooldown, &evidence, &revAfter); err != nil {
		t.Fatal(err)
	}
	if !cooldown.Equal(f.now().Add(time.Hour)) || revAfter != rev || len(evidence) < 10 {
		t.Fatal("rollback evidence", cooldown, revAfter)
	}
	if f.count(t, `SELECT count(*) FROM audit_entries WHERE action='rollout.rolled_back' AND actor_id='system:rollout' AND details->'evidence'->'operational'->0->>'variant_id'='treatment'`) != 1 ||
		f.count(t, `SELECT count(*) FROM audit_entries WHERE action='flag.updated' AND actor_id='system:rollout' AND details->>'policy'='emergency_exempt' AND after_revision=$1`, rev) != 1 {
		t.Fatal("rollback audit evidence missing")
	}
	f.advance(10 * time.Minute)
	if out := f.tick(t, p.ID); out != "inactive" {
		t.Fatal("rolled-back plan progressed", out)
	}
	cooling, err := rollouts.ActiveCooldown(f.ctx, f.pool, f.project, f.env, "listing", f.now())
	if err != nil || !cooling {
		t.Fatal("cooldown not active", cooling, err)
	}
	if cooling, _ = rollouts.ActiveCooldown(f.ctx, f.pool, f.project, f.env, "listing", f.now().Add(time.Hour)); cooling {
		t.Fatal("cooldown never ends")
	}
	if cooling, _ = rollouts.ActiveCooldown(f.ctx, f.pool, f.project, f.env, "other", f.now()); cooling {
		t.Fatal("cooldown leaked to another flag")
	}
}

func TestHealthyOrSmallSamplesNeverRollBack(t *testing.T) {
	f := setup(t, 1000)
	g := smallGuardrails
	p := f.startPlan(t, "scheduled", 2000, []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 3000}}, &g)
	f.requests(t, "treatment", 19, 19, 900) // terrible, but too few samples to decide
	for range 3 {
		if out := f.tick(t, p.ID); out == "rolled_back" || out == "breach_observed" {
			t.Fatal("rolled back on insufficient data", out)
		}
		f.advance(10 * time.Second)
	}
	f.requests(t, "treatment", 1000, 5, 100) // diluted to well within thresholds
	f.advance(time.Duration(g.WindowSeconds) * time.Second)
	f.requests(t, "treatment", 1000, 5, 100)
	if out := f.tick(t, p.ID); out != "waiting_schedule" {
		t.Fatal("healthy treatment disturbed", out)
	}
	if _, _, killed := f.traffic(t); killed {
		t.Fatal("killed healthy rollout")
	}
}

func TestConversionGuardrailWaitsForMaturedCohortsAndRequiresConsecutiveDecline(t *testing.T) {
	// Historical fixtures: finalized cohorts only; a backlog or small cohort is "insufficient".
	g := rollouts.ConversionGuardrail{MinExposed: 1000, MaxRelativeDecline: 0.2}
	cases := []struct {
		name                 string
		control, treatment   [2]int64
		lagging              bool
		breach, insufficient bool
	}{
		{"decline of 25 percent", [2]int64{1000, 100}, [2]int64{1000, 75}, false, true, false},
		{"exact 20 percent boundary", [2]int64{1000, 100}, [2]int64{1000, 80}, false, true, false},
		{"within tolerance", [2]int64{1000, 100}, [2]int64{1000, 85}, false, false, false},
		{"improvement", [2]int64{1000, 100}, [2]int64{1000, 150}, false, false, false},
		{"treatment cohort too small", [2]int64{1000, 100}, [2]int64{999, 10}, false, false, true},
		{"control cohort too small", [2]int64{999, 100}, [2]int64{1000, 10}, false, false, true},
		{"control without conversions", [2]int64{1000, 0}, [2]int64{1000, 0}, false, false, true},
		{"processing backlog", [2]int64{1000, 100}, [2]int64{1000, 10}, true, false, true},
	}
	for _, c := range cases {
		items, breach, insufficient := rollouts.EvaluateConversion(g, "control", []rollouts.Cohort{
			{ID: "control", Exposed: c.control[0], Converted: c.control[1]}, {ID: "treatment", Exposed: c.treatment[0], Converted: c.treatment[1]}}, c.lagging)
		if breach != c.breach || insufficient != c.insufficient || len(items) != 1 {
			t.Fatal(c.name, breach, insufficient, items)
		}
	}
	if _, breach, insufficient := rollouts.EvaluateConversion(g, "control", []rollouts.Cohort{{ID: "treatment", Exposed: 5000, Converted: 1}}, false); breach || !insufficient {
		t.Fatal("decided without a concurrent control")
	}
}

func TestRolloutRowsAreImmutableAndTransitionsGuarded(t *testing.T) {
	f := setup(t, 1000)
	p := f.startPlan(t, "scheduled", 2000, []rollouts.StepInput{{TrafficBP: 2000, OffsetSeconds: 60}}, nil)
	f.tick(t, p.ID) // writes a guardrail check
	for _, sql := range []string{
		`UPDATE rollout_plans SET ceiling_bp=10000 WHERE id=$1`,
		`UPDATE rollout_plans SET guardrails='{}' WHERE id=$1`,
		`UPDATE rollout_plans SET plan_hash=repeat('0',64) WHERE id=$1`,
		`UPDATE rollout_plans SET approver_id='admin1' WHERE id=$1`,
		`UPDATE rollout_plans SET state='proposed' WHERE id=$1`,
		`UPDATE rollout_plans SET expected_revision=1 WHERE id=$1`,
		`UPDATE rollout_steps SET traffic_bp=9000 WHERE plan_id=$1`,
		`DELETE FROM rollout_steps WHERE plan_id=$1`,
		`DELETE FROM rollout_plans WHERE id=$1`,
		`UPDATE guardrail_checks SET decision='pass' WHERE plan_id=$1`,
	} {
		if _, err := f.pool.Exec(f.ctx, sql, p.ID); err == nil {
			t.Fatal("guard missing for:", sql)
		}
	}
	if _, err := f.pool.Exec(f.ctx, `TRUNCATE rollout_plans CASCADE`); err == nil {
		t.Fatal("truncate allowed")
	}
	if f.count(t, `SELECT count(*) FROM users WHERE id='system:rollout' AND NOT active`) != 1 {
		t.Fatal("system actor must be an inactive user")
	}
}
