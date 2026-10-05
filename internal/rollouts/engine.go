package rollouts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"switchyard/internal/flags"
	"switchyard/pkg/evaluation"
)

type VariantEvidence struct {
	VariantID  string   `json:"variant_id"`
	Requests   int64    `json:"requests"`
	Errors     int64    `json:"errors"`
	ErrorRate  float64  `json:"error_rate"`
	P95MS      float64  `json:"p95_ms"`
	Sufficient bool     `json:"sufficient"`
	Breaches   []string `json:"breaches,omitempty"`
}
type ConversionEvidence struct {
	VariantID        string  `json:"variant_id"`
	ControlExposed   int64   `json:"control_exposed"`
	ControlConverted int64   `json:"control_converted"`
	Exposed          int64   `json:"exposed"`
	Converted        int64   `json:"converted"`
	RelativeDecline  float64 `json:"relative_decline"`
	Sufficient       bool    `json:"sufficient"`
	Breach           bool    `json:"breach"`
	Note             string  `json:"note,omitempty"`
}
type Evidence struct {
	WindowStart time.Time            `json:"window_start"`
	WindowEnd   time.Time            `json:"window_end"`
	Operational []VariantEvidence    `json:"operational"`
	Conversion  []ConversionEvidence `json:"conversion,omitempty"`
}

// evaluate reads windowed request facts for every non-control variant and, when configured,
// matured conversion cohorts. It performs no writes and holds no locks. Insufficient data is
// never a pass: it can neither promote nor roll back.
func (s *Service) evaluate(ctx context.Context, p Plan, rc runContext, now time.Time) (string, Evidence, error) {
	g := p.Guardrails
	ev := Evidence{WindowEnd: now, WindowStart: now.Add(-time.Duration(g.WindowSeconds) * time.Second)}
	if rc.Definition.Experiment == nil {
		return "", ev, errors.New("run definition missing assignment")
	}
	rows, err := s.pool.Query(ctx, `SELECT variant_id,count(*),count(*) FILTER (WHERE is_error),COALESCE(percentile_disc(0.95) WITHIN GROUP (ORDER BY latency_ms),0)
	FROM raw_events WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND kind='request_outcome' AND status='accepted' AND occurred_at>$4 AND occurred_at<=$5
	GROUP BY variant_id`, p.ProjectID, p.EnvironmentID, p.RunID, ev.WindowStart, ev.WindowEnd)
	if err != nil {
		return "", ev, err
	}
	seen := map[string]VariantEvidence{}
	for rows.Next() {
		var v VariantEvidence
		if err := rows.Scan(&v.VariantID, &v.Requests, &v.Errors, &v.P95MS); err != nil {
			rows.Close()
			return "", ev, err
		}
		seen[v.VariantID] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", ev, err
	}
	breach, insufficient := false, false
	for _, variant := range rc.Definition.Experiment.Variants {
		if variant.ID == rc.Control {
			continue
		}
		v := seen[variant.ID]
		v.VariantID = variant.ID
		v.Sufficient = v.Requests >= int64(g.MinRequests)
		if v.Requests > 0 {
			v.ErrorRate = float64(v.Errors) / float64(v.Requests)
		}
		if v.Sufficient {
			if v.ErrorRate > g.MaxErrorRate {
				v.Breaches = append(v.Breaches, "error_rate")
			}
			if v.P95MS > g.MaxP95MS {
				v.Breaches = append(v.Breaches, "latency_p95")
			}
		} else {
			insufficient = true
		}
		breach = breach || len(v.Breaches) > 0
		ev.Operational = append(ev.Operational, v)
	}
	if g.Conversion != nil {
		results, err := s.metrics.ReadForPolicy(ctx, p.ProjectID, p.RunID)
		if err != nil {
			return "", ev, err
		}
		lagging := results.Processing != nil && results.Processing.LagSeconds > float64(g.MaxCheckAgeSeconds)
		cohorts := make([]Cohort, 0, len(results.Variants))
		for _, v := range results.Variants {
			cohorts = append(cohorts, Cohort{ID: v.ID, Exposed: v.Finalized.Exposed, Converted: v.Finalized.Converted})
		}
		items, b, i := EvaluateConversion(*g.Conversion, rc.Control, cohorts, lagging)
		ev.Conversion = items
		breach, insufficient = breach || b, insufficient || i
	}
	switch {
	case breach:
		return "breach", ev, nil
	case insufficient:
		return "insufficient", ev, nil
	}
	return "pass", ev, nil
}

// Tick advances one plan by at most one step or one safety transition. Everything that can
// change configuration happens under the flag row lock (then the plan row lock), the same
// order as rollback, so progression and rollback serialize and a step that gets the lock after
// a rollback sees the killed flag and stops. It returns the outcome label.
func (s *Service) Tick(ctx context.Context, planID string) (string, error) {
	probe, err := load(ctx, s.pool, "", planID, false)
	if err != nil {
		return "", err
	}
	if probe.State != "running" && probe.State != "approved" {
		return "inactive", nil
	}
	// Evidence is gathered before locking; it only informs the decision made under the lock.
	decision, ev := "", Evidence{}
	if probe.State == "running" {
		rc, err := readRun(ctx, s.pool, probe.ProjectID, probe.RunID)
		if err != nil {
			return "", err
		}
		if decision, ev, err = s.evaluate(ctx, probe, rc, s.clock()); err != nil {
			return "", err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	def, err := flags.LockCurrent(ctx, tx, probe.ProjectID, probe.EnvironmentID, probe.FlagKey)
	if err != nil {
		return "", err
	}
	p, err := load(ctx, tx, probe.ProjectID, planID, true)
	if err != nil {
		return "", err
	}
	request := "rollout-" + p.ID + "-tick"
	now := s.clock()
	switch p.State {
	case "approved":
		if p.ApprovalExpiresAt != nil && !now.Before(*p.ApprovalExpiresAt) {
			return s.commit(ctx, tx, "expired", s.finish(ctx, tx, SystemActor, "system", p, "expired", "rollout.expired", "approval expired before start", request, nil))
		}
		if def.Revision != p.BaseRevision || def.Killed {
			return s.commit(ctx, tx, "stale", s.finish(ctx, tx, SystemActor, "system", p, "stale", "rollout.stale", "configuration changed before start", request, nil))
		}
		return "waiting_start", nil
	case "running":
	default:
		return "inactive", nil
	}
	if def.Killed {
		return s.commit(ctx, tx, "cancelled", s.finish(ctx, tx, SystemActor, "system", p, "cancelled", "rollout.cancelled", "flag was disabled; scheduled progression cancelled", request, map[string]any{"revision": def.Revision}))
	}
	if p.ExpectedRevision == nil || def.Revision != *p.ExpectedRevision || def.Experiment == nil || def.Experiment.RunID != p.RunID {
		return s.commit(ctx, tx, "stale", s.finish(ctx, tx, SystemActor, "system", p, "stale", "rollout.stale", "configuration changed outside the approved plan", request, map[string]any{"revision": def.Revision}))
	}
	evidence, err := json.Marshal(ev)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO guardrail_checks(plan_id,checked_at,decision,evidence) VALUES($1,$2,$3,$4)`, p.ID, now, decision, evidence); err != nil {
		return "", err
	}
	if decision == "breach" {
		var breaches int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT decision FROM guardrail_checks WHERE plan_id=$1 ORDER BY id DESC LIMIT $2) t WHERE decision='breach'`, p.ID, p.Guardrails.ConsecutiveBreaches).Scan(&breaches); err != nil {
			return "", err
		}
		if breaches >= p.Guardrails.ConsecutiveBreaches {
			return s.commit(ctx, tx, "rolled_back", s.rollbackLocked(ctx, tx, p, def, ev, request))
		}
		return s.commit(ctx, tx, "breach_observed", nil)
	}
	var next *Step
	for i := range p.Steps {
		if p.Steps[i].State == "pending" {
			next = &p.Steps[i]
			break
		}
	}
	if next == nil {
		return s.commit(ctx, tx, "completed", s.finish(ctx, tx, SystemActor, "system", p, "completed", "rollout.completed", "all approved steps applied", request, nil))
	}
	if next.DueAt == nil || now.Before(*next.DueAt) {
		return s.commit(ctx, tx, "waiting_schedule", nil)
	}
	if p.Mode == "metric" && decision != "pass" {
		// Insufficient or unhealthy evidence never promotes; the plan simply waits.
		return s.commit(ctx, tx, "waiting_metrics", nil)
	}
	if next.TrafficBP > p.CeilingBP || next.TrafficBP <= def.Experiment.TrafficBP || next.TrafficBP-def.Experiment.TrafficBP > MaxStepIncreaseBP {
		return s.commit(ctx, tx, "stale", s.finish(ctx, tx, SystemActor, "system", p, "stale", "rollout.stale", "step is outside the approved bounds for the current traffic", request, map[string]any{"ordinal": next.Ordinal}))
	}
	changed, err := s.flags.ApplyTraffic(ctx, tx, SystemActor, p.ProjectID, flags.TrafficChange{
		EnvironmentID: p.EnvironmentID, Key: p.FlagKey, ExpectedRevision: *p.ExpectedRevision, TrafficBP: next.TrafficBP,
		Reason: fmt.Sprintf("approved rollout plan step %d", next.Ordinal), Source: "system",
		Details: map[string]any{"plan_id": p.ID, "plan_hash": p.PlanHash, "ordinal": next.Ordinal, "approver_id": p.ApproverID, "guardrail": decision},
	}, request+"-"+fmt.Sprint(next.Ordinal))
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE rollout_steps SET state='applied',applied_revision=$3,applied_at=$4 WHERE plan_id=$1 AND ordinal=$2 AND state='pending'`, p.ID, next.Ordinal, changed.Revision, now); err != nil {
		return "", err
	}
	if _, err = tx.Exec(ctx, `UPDATE rollout_plans SET expected_revision=$2 WHERE id=$1`, p.ID, changed.Revision); err != nil {
		return "", err
	}
	outcome := "step_applied"
	if next.Ordinal == p.Steps[len(p.Steps)-1].Ordinal {
		if err = s.finish(ctx, tx, SystemActor, "system", p, "completed", "rollout.completed", "all approved steps applied", request, map[string]any{"final_revision": changed.Revision}); err != nil {
			return "", err
		}
		outcome = "completed"
	}
	return s.commit(ctx, tx, outcome, nil)
}

func (s *Service) commit(ctx context.Context, tx pgx.Tx, outcome string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if err = tx.Commit(ctx); err != nil {
		return "", err
	}
	return outcome, nil
}

// rollbackLocked disables the flag to its safe value, cancels all remaining steps, records the
// measured evidence, advances the revision and starts the cooldown, in the caller's transaction.
// The caller holds the flag and plan row locks.
func (s *Service) rollbackLocked(ctx context.Context, tx pgx.Tx, p Plan, def evaluation.Definition, ev Evidence, requestID string) error {
	summary := breachSummary(ev)
	killed, wrote, err := s.flags.Kill(ctx, tx, SystemActor, p.ProjectID, p.EnvironmentID, p.FlagKey, "automatic safety rollback: "+summary, "system", requestID, map[string]any{"plan_id": p.ID, "plan_hash": p.PlanHash})
	if err != nil {
		return err
	}
	if !wrote {
		return errors.New("rollback found the flag already disabled under the lock")
	}
	evidence, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	now := s.clock()
	if _, err = tx.Exec(ctx, `INSERT INTO safety_rollbacks(plan_id,project_id,environment_id,flag_key,run_id,revision_before,revision_after,rolled_back_at,cooldown_until,evidence)
	VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, p.ID, p.ProjectID, p.EnvironmentID, p.FlagKey, p.RunID, def.Revision, killed.Revision, now, now.Add(time.Duration(p.Guardrails.CooldownSeconds)*time.Second), evidence); err != nil {
		return err
	}
	return s.finish(ctx, tx, SystemActor, "system", p, "rolled_back", "rollout.rolled_back", "automatic safety rollback: "+summary, requestID, map[string]any{"revision_before": def.Revision, "revision_after": killed.Revision, "evidence": ev})
}

func breachSummary(ev Evidence) string {
	for _, v := range ev.Operational {
		if len(v.Breaches) > 0 {
			return fmt.Sprintf("variant %s %v (error rate %.4f, p95 %.0f ms over %d requests)", v.VariantID, v.Breaches, v.ErrorRate, v.P95MS, v.Requests)
		}
	}
	for _, c := range ev.Conversion {
		if c.Breach {
			return fmt.Sprintf("variant %s conversion declined %.1f%% relative to control", c.VariantID, c.RelativeDecline*100)
		}
	}
	return "guardrail breach"
}

// ActiveCooldown reports whether a safety rollback of the flag is still cooling down.
func ActiveCooldown(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, projectID, environmentID, flagKey string, now time.Time) (bool, error) {
	var active bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM safety_rollbacks WHERE project_id=$1 AND environment_id=$2 AND flag_key=$3 AND cooldown_until>$4)`, projectID, environmentID, flagKey, now).Scan(&active)
	return active, err
}

// RunOnce ticks every approved or running plan (bounded) and reports outcome counts. Each plan
// is isolated: one failing plan does not stop the others.
func (s *Service) RunOnce(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM rollout_plans WHERE state IN ('approved','running') ORDER BY id LIMIT 200`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts := map[string]int{}
	var failures error
	for _, id := range ids {
		if ctx.Err() != nil {
			return counts, ctx.Err()
		}
		outcome, err := s.Tick(ctx, id)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("plan %s: %w", id, err))
			counts["error"]++
			continue
		}
		counts[outcome]++
	}
	return counts, failures
}

type Cohort struct {
	ID                 string
	Exposed, Converted int64
}

// EvaluateConversion compares each treatment's matured (finalized) cohort with the concurrent
// control. It never decides from small cohorts, a control without conversions, a missing
// control or a processing backlog: those are "insufficient", which neither promotes nor rolls back.
func EvaluateConversion(g ConversionGuardrail, control string, cohorts []Cohort, lagging bool) (items []ConversionEvidence, breach, insufficient bool) {
	var base *Cohort
	for i := range cohorts {
		if cohorts[i].ID == control {
			base = &cohorts[i]
		}
	}
	for _, v := range cohorts {
		if v.ID == control {
			continue
		}
		c := ConversionEvidence{VariantID: v.ID, Exposed: v.Exposed, Converted: v.Converted}
		if base != nil {
			c.ControlExposed, c.ControlConverted = base.Exposed, base.Converted
		}
		switch {
		case lagging:
			c.Note = "processing backlog"
		case base == nil:
			c.Note = "no concurrent control"
		case c.ControlExposed < g.MinExposed || c.Exposed < g.MinExposed:
			c.Note = "matured cohort too small"
		case c.ControlConverted == 0:
			c.Note = "control has no conversions"
		default:
			c.Sufficient = true
			controlRate := float64(c.ControlConverted) / float64(c.ControlExposed)
			rate := float64(c.Converted) / float64(c.Exposed)
			c.RelativeDecline = (controlRate - rate) / controlRate
			c.Breach = c.RelativeDecline >= g.MaxRelativeDecline
		}
		breach = breach || c.Breach
		insufficient = insufficient || !c.Sufficient
		items = append(items, c)
	}
	return items, breach, insufficient
}
