//go:build integration

package proposals_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"switchyard/internal/auth"
	"switchyard/internal/flags"
	"switchyard/internal/platform/postgres"
	"switchyard/internal/projects"
	"switchyard/internal/proposals"
	"switchyard/internal/testutil"
	"switchyard/migrations"
	"switchyard/pkg/evaluation"
)

func value(v string) evaluation.Value {
	return evaluation.Value{Type: "boolean", Data: json.RawMessage(v)}
}

type fixture struct {
	pool              *pgxpool.Pool
	ctx               context.Context
	project, dev, prd string
	clock             *atomic.Int64
	ps                *proposals.Service
	fs                *flags.Service
}

var (
	admin1    = auth.Actor{ID: "admin1"}
	admin2    = auth.Actor{ID: "admin2"}
	developer = auth.Actor{ID: "developer"}
	viewer    = auth.Actor{ID: "viewer"}
)

func setup(t *testing.T) *fixture {
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
	p, err := ps.Create(ctx, admin1, "Approvals", "project-request")
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
	f.ps = proposals.New(pool, func() time.Time { return time.Unix(0, f.clock.Load()).UTC() })
	for name, dst := range map[string]*string{"development": &f.dev, "production": &f.prd} {
		if err := pool.QueryRow(ctx, `SELECT id FROM environments WHERE project_id=$1 AND name=$2`, p.ID, name).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func (f *fixture) advance(d time.Duration) { f.clock.Add(int64(d)) }
func (f *fixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func rollout(bp int, enabled bool) flags.Configuration {
	return flags.Configuration{Default: value("false"), Safe: value("false"), Rollout: &evaluation.Rollout{TrafficBP: bp, Value: value("true")}, Killed: !enabled}
}
func (f *fixture) propose(t *testing.T, actor auth.Actor, key string, base int64, c flags.Configuration) (proposals.Proposal, error) {
	t.Helper()
	return f.ps.Create(f.ctx, actor, f.project, proposals.CreateInput{EnvironmentID: f.prd, FlagKey: key, Kind: "update", BaseRevision: base, Configuration: c, Rationale: "change " + key}, "req-propose")
}

// release creates a production flag through the full review path (ends at revision 1).
func (f *fixture) release(t *testing.T, key string, bp int) {
	t.Helper()
	p, err := f.ps.Create(f.ctx, developer, f.project, proposals.CreateInput{EnvironmentID: f.prd, FlagKey: key, Kind: "create", FlagType: "boolean", Configuration: rollout(bp, true), Rationale: "launch " + key}, "req-create-"+key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Approve(f.ctx, admin2, f.project, p.ID, p.DiffHash, "reviewed", "req-approve"); err != nil {
		t.Fatal(err)
	}
	applied, err := f.ps.Apply(f.ctx, developer, f.project, p.ID, "req-apply")
	if err != nil || applied.State != "applied" || *applied.AppliedRevision != 1 {
		t.Fatal("release", applied, err)
	}
}
func (f *fixture) direct(key string, rev int64, c flags.Configuration, actor auth.Actor) (evaluation.Definition, error) {
	return f.fs.Update(f.ctx, actor, f.project, key, flags.UpdateInput{EnvironmentID: f.prd, ExpectedRevision: rev, Configuration: c, Reason: "direct production change"}, "req-direct")
}

func TestProductionDirectPolicyAllowsOnlyEmergencyReductions(t *testing.T) {
	f := setup(t)
	f.release(t, "listing", 3000)
	if _, err := f.fs.Create(f.ctx, admin1, f.project, flags.CreateInput{EnvironmentID: f.prd, Key: "other", Type: "boolean", Configuration: rollout(100, true), Reason: "bypass"}, "r"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("direct production create allowed", err)
	}
	for name, c := range map[string]flags.Configuration{
		"increase": rollout(4000, true),
		"default":  {Default: value("true"), Safe: value("false"), Rollout: &evaluation.Rollout{TrafficBP: 3000, Value: value("true")}},
		"value":    {Default: value("false"), Safe: value("false"), Rollout: &evaluation.Rollout{TrafficBP: 3000, Value: value("false")}},
		"remove":   {Default: value("false"), Safe: value("false")},
	} {
		if _, err := f.direct("listing", 1, c, admin1); !errors.Is(err, auth.ErrForbidden) {
			t.Fatal("non-exempt direct production change allowed:", name, err)
		}
	}
	if _, err := f.direct("listing", 1, rollout(1000, true), viewer); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("viewer reduced traffic")
	}
	// Denied attempts leave no revision or audit trace.
	if f.count(t, `SELECT count(*) FROM flag_revisions WHERE environment_id=$1`, f.prd) != 1 || f.count(t, `SELECT count(*) FROM audit_entries WHERE action='flag.updated' AND environment_id=$1`, f.prd) != 0 {
		t.Fatal("denied production changes left durable state")
	}
	d, err := f.direct("listing", 1, rollout(1000, true), developer)
	if err != nil || d.Revision != 2 || d.Rollout.TrafficBP != 1000 {
		t.Fatal("traffic decrease", d, err)
	}
	d, err = f.direct("listing", 2, rollout(1000, false), developer)
	if err != nil || !d.Killed || d.Revision != 3 {
		t.Fatal("kill", d, err)
	}
	if f.count(t, `SELECT count(*) FROM audit_entries WHERE details->>'policy'='emergency_exempt' AND environment_id=$1`, f.prd) != 2 {
		t.Fatal("exempt policy decisions not audited")
	}
	if _, err := f.direct("listing", 3, rollout(1000, true), admin1); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("re-enable bypassed review", err)
	}
	if err := auth.Authorize(f.ctx, f.pool, admin1, f.project, f.prd, true); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("generic production write gate open")
	}
}

func TestProposalLifecycleExactDiffSeparationAndIdempotentApply(t *testing.T) {
	f := setup(t)
	f.release(t, "listing", 1000)
	if _, err := f.propose(t, viewer, "listing", 1, rollout(2000, true)); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("viewer proposed", err)
	}
	if _, err := f.ps.Create(f.ctx, developer, f.project, proposals.CreateInput{EnvironmentID: f.dev, FlagKey: "listing", Kind: "update", BaseRevision: 1, Configuration: rollout(2000, true), Rationale: "dev"}, "r"); !errors.Is(err, auth.ErrInvalid) {
		t.Fatal("non-production proposal accepted", err)
	}
	if _, err := f.propose(t, developer, "listing", 1, rollout(1000, true)); !errors.Is(err, auth.ErrInvalid) {
		t.Fatal("no-op proposal accepted", err)
	}
	if _, err := f.propose(t, developer, "listing", 0, rollout(2000, true)); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("wrong base revision accepted", err)
	}
	if _, err := f.propose(t, developer, "missing", 1, rollout(2000, true)); !errors.Is(err, flags.ErrNotFound) {
		t.Fatal("proposal for unknown flag accepted", err)
	}
	p, err := f.propose(t, developer, "listing", 1, rollout(2000, true))
	if err != nil || p.State != "validated" || len(p.DiffHash) != 64 {
		t.Fatal("propose", p, err)
	}
	if _, err := f.ps.Apply(f.ctx, developer, f.project, p.ID, "r"); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("unapproved proposal applied", err)
	}
	if _, err := f.ps.Approve(f.ctx, developer, f.project, p.ID, p.DiffHash, "dev", "r"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("developer approved", err)
	}
	// A second proposal by an admin cannot be approved by that same admin or against another diff.
	q, err := f.propose(t, admin2, "listing", 1, rollout(1500, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Approve(f.ctx, admin2, f.project, q.ID, q.DiffHash, "self", "r"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("proposer approved own change", err)
	}
	if _, err := f.ps.Approve(f.ctx, admin1, f.project, q.ID, p.DiffHash, "wrong diff", "r"); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("approval of a different diff accepted", err)
	}
	if _, err := f.ps.Approve(f.ctx, admin1, f.project, p.ID, p.DiffHash, "reviewed", "r"); err != nil {
		t.Fatal("distinct admin could not approve", err)
	}
	var wg sync.WaitGroup
	results := make(chan proposals.Proposal, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			got, err := f.ps.Apply(f.ctx, developer, f.project, p.ID, "req-apply")
			results <- got
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent apply error", err)
		}
	}
	for got := range results {
		if got.State != "applied" || *got.AppliedRevision != 2 {
			t.Fatal("applied result differs", got)
		}
	}
	d, err := f.fs.Get(f.ctx, f.project, f.prd, "listing")
	if err != nil || d.Revision != 2 || d.Rollout.TrafficBP != 2000 {
		t.Fatal("configuration", d, err)
	}
	if f.count(t, `SELECT count(*) FROM audit_entries WHERE action='proposal.applied'`) != 2 { // release + this one
		t.Fatal("duplicate apply audit")
	}
	if f.count(t, `SELECT count(*) FROM flag_revisions WHERE environment_id=$1`, f.prd) != 2 {
		t.Fatal("duplicate revisions")
	}
	if f.count(t, `SELECT count(*) FROM audit_entries WHERE action='flag.updated' AND details->>'proposal_id'=$1 AND details->>'approver_id'='admin1' AND details->>'proposer_id'='developer'`, p.ID) != 1 {
		t.Fatal("applied change lacks approval evidence")
	}
	// q was based on revision 1, which p superseded: it cannot be approved or applied.
	if _, err := f.ps.Approve(f.ctx, admin1, f.project, q.ID, q.DiffHash, "late", "r"); !errors.Is(err, proposals.ErrStale) {
		t.Fatal("stale proposal approved", err)
	}
	if got, _ := f.ps.Get(f.ctx, admin1, f.project, q.ID); got.State != "stale" {
		t.Fatal("stale state not recorded", got.State)
	}
	if _, err := f.ps.Apply(f.ctx, developer, f.project, q.ID, "r"); !errors.Is(err, proposals.ErrStale) {
		t.Fatal("stale proposal applied", err)
	}
}

func TestApprovalExpiryConflictingRevisionAndDemotedApprover(t *testing.T) {
	f := setup(t)
	f.release(t, "listing", 1000)
	p, err := f.propose(t, developer, "listing", 1, rollout(2000, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Approve(f.ctx, admin2, f.project, p.ID, p.DiffHash, "ok", "r"); err != nil {
		t.Fatal(err)
	}
	f.advance(proposals.ApprovalTTL - time.Nanosecond)
	f.advance(time.Nanosecond) // exactly 24 hours: the approval is no longer valid
	if _, err := f.ps.Apply(f.ctx, developer, f.project, p.ID, "r"); !errors.Is(err, proposals.ErrExpired) {
		t.Fatal("expired approval applied", err)
	}
	if got, _ := f.ps.Get(f.ctx, admin1, f.project, p.ID); got.State != "expired" {
		t.Fatal("expiry not recorded", got.State)
	}
	if d, _ := f.fs.Get(f.ctx, f.project, f.prd, "listing"); d.Revision != 1 {
		t.Fatal("expired approval changed configuration")
	}
	// A one-nanosecond-early apply is still valid.
	early, err := f.propose(t, developer, "listing", 1, rollout(1800, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Approve(f.ctx, admin2, f.project, early.ID, early.DiffHash, "ok", "r"); err != nil {
		t.Fatal(err)
	}
	f.advance(proposals.ApprovalTTL - time.Nanosecond)
	if got, err := f.ps.Apply(f.ctx, developer, f.project, early.ID, "r"); err != nil || got.State != "applied" {
		t.Fatal("approval just inside the window rejected", got, err)
	}
	// An emergency kill after approval invalidates the approval; apply must not override it.
	r, err := f.propose(t, developer, "listing", 2, rollout(3000, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Approve(f.ctx, admin2, f.project, r.ID, r.DiffHash, "ok", "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.direct("listing", 2, rollout(1800, false), developer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Apply(f.ctx, developer, f.project, r.ID, "r"); !errors.Is(err, proposals.ErrStale) {
		t.Fatal("approval survived a conflicting revision", err)
	}
	if d, _ := f.fs.Get(f.ctx, f.project, f.prd, "listing"); !d.Killed || d.Revision != 3 {
		t.Fatal("stale apply overrode the kill switch", d)
	}
	// Re-enablement is reviewed, and an approval is void once its approver loses the admin role.
	reenable, err := f.propose(t, developer, "listing", 3, rollout(1800, true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Approve(f.ctx, admin2, f.project, reenable.ID, reenable.DiffHash, "ok", "r"); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE users SET role='developer' WHERE id='admin2'`)
	if _, err := f.ps.Apply(f.ctx, developer, f.project, reenable.ID, "r"); !errors.Is(err, auth.ErrForbidden) {
		t.Fatal("demoted approver's approval honored", err)
	}
	if d, _ := f.fs.Get(f.ctx, f.project, f.prd, "listing"); !d.Killed {
		t.Fatal("re-enabled without a valid approval")
	}
	if _, err := f.ps.Reject(f.ctx, admin1, f.project, reenable.ID, "no longer wanted", "r"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ps.Apply(f.ctx, developer, f.project, reenable.ID, "r"); !errors.Is(err, auth.ErrConflict) {
		t.Fatal("rejected proposal applied", err)
	}
}

func TestProposalRowsAreImmutableAndTransitionsAreGuarded(t *testing.T) {
	f := setup(t)
	f.release(t, "listing", 1000)
	p, err := f.propose(t, developer, "listing", 1, rollout(2000, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE proposals SET diff_hash=repeat('0',64) WHERE id=$1`,
		`UPDATE proposals SET configuration='{}' WHERE id=$1`,
		`UPDATE proposals SET state='applied',applied_revision=9,approver_id='admin1',approved_at=now(),approval_expires_at=now()+interval '1 hour' WHERE id=$1`,
		`UPDATE proposals SET state='approved',approver_id='developer',approved_at=now(),approval_expires_at=now()+interval '1 hour' WHERE id=$1`,
		`DELETE FROM proposals WHERE id=$1`,
	} {
		if _, err := f.pool.Exec(f.ctx, sql, p.ID); err == nil {
			t.Fatal("proposal guard missing for:", sql)
		}
	}
	if _, err := f.pool.Exec(f.ctx, `TRUNCATE proposals`); err == nil {
		t.Fatal("truncate allowed")
	}
}
