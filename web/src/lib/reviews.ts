import type { Rollout, Rule, Value } from "./flags";

export type Configuration = {
  default: Value;
  safe: Value;
  rules: Rule[] | null;
  rollout?: Rollout | null;
  killed: boolean;
};
export type ProposalState =
  "validated" | "approved" | "applied" | "rejected" | "expired" | "stale";
export type Proposal = {
  id: string;
  environment_id: string;
  flag_key: string;
  kind: "create" | "update";
  type: Value["type"];
  base_revision: number;
  configuration: Configuration;
  diff: { before: Configuration | null; after: Configuration };
  diff_hash: string;
  rationale: string;
  proposer_id: string;
  source: "human" | "agent";
  state: ProposalState;
  approver_id?: string;
  approval_expires_at?: string;
  applied_revision?: number;
  created_at: string;
};
export type PlanState =
  | "proposed"
  | "approved"
  | "running"
  | "completed"
  | "cancelled"
  | "rolled_back"
  | "stale"
  | "rejected"
  | "expired";
export type PlanStep = {
  ordinal: number;
  traffic_bp: number;
  offset_seconds: number;
  due_at?: string;
  state: "pending" | "applied" | "cancelled";
  applied_revision?: number;
};
export type RolloutPlan = {
  id: string;
  environment_id: string;
  flag_key: string;
  run_id: string;
  mode: "scheduled" | "metric";
  ceiling_bp: number;
  base_revision: number;
  plan_hash: string;
  rationale: string;
  proposer_id: string;
  state: PlanState;
  approver_id?: string;
  approval_expires_at?: string;
  finish_reason: string;
  created_at: string;
  steps: PlanStep[];
};
export type GuardrailCheck = {
  id: number;
  checked_at: string;
  decision: "pass" | "insufficient" | "breach";
  evidence: {
    operational?: Array<{
      variant_id: string;
      requests: number;
      errors: number;
      error_rate: number;
      p95_ms: number;
      sufficient: boolean;
      breaches?: string[];
    }>;
  };
};

export const openProposal = (s: ProposalState) =>
  s === "validated" || s === "approved";
export const livePlan = (s: PlanState) =>
  s === "proposed" || s === "approved" || s === "running";

// Separation of duties is enforced by Go; the UI only avoids offering actions that cannot work.
export function canApprove(
  role: string,
  userID: string,
  proposerID: string,
): boolean {
  return role === "admin" && userID !== proposerID;
}

function describe(c: Configuration): string[] {
  const lines = [
    `default ${JSON.stringify(c.default.data)}`,
    `safe value ${JSON.stringify(c.safe.data)}`,
    c.killed ? "disabled (kill switch)" : "enabled",
  ];
  if (c.rollout)
    lines.push(
      `rollout ${(c.rollout.traffic_bp / 100).toFixed(2)}% → ${JSON.stringify(c.rollout.value.data)}`,
    );
  lines.push(`${c.rules?.length ?? 0} targeting rule(s)`);
  return lines;
}

// A readable before/after of the exact reviewed change. Approval is still bound to diff_hash,
// not to this summary.
export function summarizeDiff(diff: Proposal["diff"]): {
  before: string[];
  after: string[];
  changed: string[];
} {
  const before = diff.before ? describe(diff.before) : [];
  const after = describe(diff.after);
  const changed = after.filter((line) => !before.includes(line));
  return { before, after, changed };
}

export function stepSummary(step: PlanStep): string {
  const pct = `${(step.traffic_bp / 100).toFixed(2)}%`;
  if (step.state === "applied")
    return `${pct} · applied at revision ${step.applied_revision}`;
  if (step.state === "cancelled") return `${pct} · cancelled`;
  return step.due_at
    ? `${pct} · due ${new Date(step.due_at).toLocaleTimeString()}`
    : `${pct} · after ${step.offset_seconds}s`;
}

export function checkSummary(check: GuardrailCheck): string {
  const worst =
    check.evidence.operational?.find((v) => v.breaches?.length) ??
    check.evidence.operational?.[0];
  if (!worst) return check.decision;
  const rate = `${(100 * worst.error_rate).toFixed(2)}%`;
  return `${check.decision}: ${worst.variant_id} ${worst.requests} requests, ${rate} errors, p95 ${worst.p95_ms} ms${worst.sufficient ? "" : " (not enough requests to decide)"}`;
}
