"use client";
import { useCallback, useEffect, useState } from "react";
import { api, errorMessage } from "@/lib/api";
import type { Environment, Session } from "@/lib/api";
import {
  canApprove,
  checkSummary,
  livePlan,
  openProposal,
  stepSummary,
  summarizeDiff,
} from "@/lib/reviews";
import type { GuardrailCheck, Proposal, RolloutPlan } from "@/lib/reviews";

type Action = { label: string; run: () => Promise<unknown>; danger?: boolean };

function Actions({
  actions,
  busy,
  onRun,
}: {
  actions: Action[];
  busy: boolean;
  onRun: (action: Action) => void;
}) {
  return (
    <div className="actions">
      {actions.map((a) => (
        <button
          key={a.label}
          className={a.danger ? "secondary danger" : undefined}
          disabled={busy}
          onClick={() => onRun(a)}
        >
          {a.label}
        </button>
      ))}
    </div>
  );
}

function ProposalCard({
  proposal,
  session,
  reason,
  busy,
  projectID,
  onRun,
}: {
  proposal: Proposal;
  session: Session;
  reason: string;
  busy: boolean;
  projectID: string;
  onRun: (action: Action) => void;
}) {
  const diff = summarizeDiff(proposal.diff);
  const post = (verb: string, body?: unknown) => () =>
    api(`/v1/projects/${projectID}/proposals/${proposal.id}/${verb}`, {
      method: "POST",
      csrf: session.csrf_token,
      body,
    });
  const actions: Action[] = [];
  const { role, id } = session.user;
  if (
    proposal.state === "validated" &&
    canApprove(role, id, proposal.proposer_id)
  )
    actions.push({
      label: "Approve this exact diff",
      run: post("approve", { diff_hash: proposal.diff_hash, reason }),
    });
  if (proposal.state === "approved" && role !== "viewer")
    actions.push({ label: "Apply", run: post("apply") });
  if (
    openProposal(proposal.state) &&
    (role === "admin" || id === proposal.proposer_id)
  )
    actions.push({
      label: id === proposal.proposer_id ? "Withdraw" : "Reject",
      run: post("reject", { reason }),
      danger: true,
    });
  return (
    <article className="review" aria-label={`Proposal ${proposal.flag_key}`}>
      <h3>
        {proposal.flag_key}{" "}
        <small>
          {proposal.kind} · base revision {proposal.base_revision}
        </small>
      </h3>
      <p>
        <strong>{proposal.state}</strong> · proposed by {proposal.proposer_id}
        {proposal.approver_id ? ` · approved by ${proposal.approver_id}` : ""}
        {proposal.approval_expires_at && proposal.state === "approved"
          ? ` · approval expires ${new Date(proposal.approval_expires_at).toLocaleString()}`
          : ""}
        {proposal.applied_revision
          ? ` · applied at revision ${proposal.applied_revision}`
          : ""}
      </p>
      <p className="muted">{proposal.rationale}</p>
      <div className="columns">
        <div>
          <h4>Before</h4>
          <ul>
            {diff.before.length ? (
              diff.before.map((line) => <li key={line}>{line}</li>)
            ) : (
              <li>new flag</li>
            )}
          </ul>
        </div>
        <div>
          <h4>After</h4>
          <ul>
            {diff.after.map((line) => (
              <li key={line}>
                {diff.changed.includes(line) ? <mark>{line}</mark> : line}
              </li>
            ))}
          </ul>
        </div>
      </div>
      <small>
        Approval binds to diff <code>{proposal.diff_hash.slice(0, 16)}…</code>
      </small>
      <Actions actions={actions} busy={busy} onRun={onRun} />
    </article>
  );
}

function PlanCard({
  plan,
  session,
  reason,
  busy,
  projectID,
  onRun,
}: {
  plan: RolloutPlan;
  session: Session;
  reason: string;
  busy: boolean;
  projectID: string;
  onRun: (action: Action) => void;
}) {
  const [checks, setChecks] = useState<GuardrailCheck[] | null>(null);
  const [error, setError] = useState("");
  const post = (verb: string, body?: unknown) => () =>
    api(`/v1/projects/${projectID}/rollouts/${plan.id}/${verb}`, {
      method: "POST",
      csrf: session.csrf_token,
      body,
    });
  const { role, id } = session.user;
  const actions: Action[] = [];
  if (plan.state === "proposed" && canApprove(role, id, plan.proposer_id))
    actions.push({
      label: "Approve this exact plan",
      run: post("approve", { plan_hash: plan.plan_hash, reason }),
    });
  if (plan.state === "approved" && role !== "viewer")
    actions.push({ label: "Start", run: post("start") });
  if (
    (plan.state === "proposed" || plan.state === "approved") &&
    (role === "admin" || id === plan.proposer_id)
  )
    actions.push({
      label: "Reject",
      run: post("reject", { reason }),
      danger: true,
    });
  if (
    (plan.state === "approved" || plan.state === "running") &&
    role !== "viewer"
  )
    actions.push({
      label: "Cancel plan",
      run: post("cancel", { reason }),
      danger: true,
    });
  async function loadChecks() {
    setError("");
    try {
      setChecks(
        await api<GuardrailCheck[]>(
          `/v1/projects/${projectID}/rollouts/${plan.id}/checks`,
        ),
      );
    } catch (e) {
      setError(errorMessage(e));
    }
  }
  return (
    <article className="review" aria-label={`Rollout plan ${plan.flag_key}`}>
      <h3>
        {plan.flag_key}{" "}
        <small>
          {plan.mode} · ceiling {(plan.ceiling_bp / 100).toFixed(2)}%
        </small>
      </h3>
      <p>
        <strong>{plan.state}</strong> · proposed by {plan.proposer_id}
        {plan.approver_id ? ` · approved by ${plan.approver_id}` : ""}
        {plan.finish_reason ? ` · ${plan.finish_reason}` : ""}
      </p>
      <p className="muted">{plan.rationale}</p>
      <ol>
        {plan.steps.map((step) => (
          <li key={step.ordinal}>{stepSummary(step)}</li>
        ))}
      </ol>
      {plan.state === "rolled_back" && (
        <p className="notice error" role="status">
          Automatic safety rollback: the flag was disabled to its safe value and
          remaining steps were cancelled. Re-enabling needs a reviewed change
          after the cooldown.
        </p>
      )}
      <div className="actions">
        <button className="secondary" onClick={() => void loadChecks()}>
          Guardrail checks
        </button>
      </div>
      {error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}
      {checks &&
        (checks.length ? (
          <ul>
            {checks.slice(0, 8).map((c) => (
              <li key={c.id}>
                <time dateTime={c.checked_at}>
                  {new Date(c.checked_at).toLocaleTimeString()}
                </time>{" "}
                {checkSummary(c)}
              </li>
            ))}
          </ul>
        ) : (
          <p className="muted">No checks recorded yet.</p>
        ))}
      <Actions actions={actions} busy={busy} onRun={onRun} />
    </article>
  );
}

export default function ReviewsPanel({
  projectID,
  environment,
  session,
}: {
  projectID: string;
  environment: Environment;
  session: Session;
}) {
  const [proposals, setProposals] = useState<Proposal[]>([]);
  const [plans, setPlans] = useState<RolloutPlan[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [reason, setReason] = useState("");
  const [refresh, setRefresh] = useState(0);
  const query = `?environment_id=${environment.id}`;
  const load = useCallback(
    async (signal?: AbortSignal) => {
      const [p, r] = await Promise.all([
        api<Proposal[]>(`/v1/projects/${projectID}/proposals${query}`, {
          signal,
        }),
        api<RolloutPlan[]>(`/v1/projects/${projectID}/rollouts${query}`, {
          signal,
        }),
      ]);
      setProposals(p);
      setPlans(r);
    },
    [projectID, query],
  );
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setError("");
    load(controller.signal)
      .catch((e) => {
        if (!controller.signal.aborted) setError(errorMessage(e));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    // Plans progress in the worker; poll so a reviewer sees steps and rollbacks promptly.
    const timer = setInterval(() => {
      load(controller.signal).catch(() => undefined);
    }, 5000);
    return () => {
      controller.abort();
      clearInterval(timer);
    };
  }, [load, refresh]);
  async function run(action: Action) {
    if (!reason.trim()) {
      setError("Enter a reason first; it is recorded in the audit history.");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await action.run();
      setRefresh((n) => n + 1);
    } catch (e) {
      setError(errorMessage(e));
      setRefresh((n) => n + 1);
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="panel">
      <div className="section-heading">
        <div>
          <h2>Reviews</h2>
          <p className="muted">
            Production changes need an exact-diff approval from a different
            admin. Rollout plans progress only inside their approved bounds and
            roll back automatically on a sustained guardrail breach.
          </p>
        </div>
        <button className="secondary" onClick={() => setRefresh((n) => n + 1)}>
          Refresh
        </button>
      </div>
      <label>
        Reason for the next action (recorded in the audit history)
        <input
          aria-label="Review reason"
          value={reason}
          maxLength={512}
          onChange={(e) => setReason(e.target.value)}
        />
      </label>
      {error && (
        <p className="notice error" role="alert">
          {error}
        </p>
      )}
      {loading ? (
        <p role="status">Loading reviews…</p>
      ) : (
        <>
          <h3>Production proposals</h3>
          {proposals.length ? (
            proposals.map((p) => (
              <ProposalCard
                key={p.id}
                proposal={p}
                session={session}
                reason={reason}
                busy={busy}
                projectID={projectID}
                onRun={(a) => void run(a)}
              />
            ))
          ) : (
            <p className="empty">
              No proposals in {environment.name}.
              {environment.name !== "production"
                ? " Only production changes use proposals."
                : ""}
            </p>
          )}
          <h3>Rollout plans</h3>
          {plans.length ? (
            plans.map((p) => (
              <PlanCard
                key={p.id}
                plan={p}
                session={session}
                reason={reason}
                busy={busy}
                projectID={projectID}
                onRun={(a) => void run(a)}
              />
            ))
          ) : (
            <p className="empty">No rollout plans in {environment.name}.</p>
          )}
          {plans.some((p) => livePlan(p.state)) && (
            <p className="muted">
              Live plans are checked by the worker every 10 seconds.
            </p>
          )}
        </>
      )}
    </section>
  );
}
