import type { Flag, Value } from "./flags";
export type Variant = {
  id: string;
  ordinal: number;
  weight_bp: number;
  value: Value;
};
export type Run = {
  id: string;
  name: string;
  state: "draft" | "running" | "paused" | "completed";
  control_variant_id: string;
  definition: Flag & {
    experiment: { run_id: string; traffic_bp: number; variants: Variant[] };
  };
  configuration_revision: number;
  created_at: string;
  started_at?: string;
  completed_at?: string;
};
export type Cohort = "provisional" | "finalized" | "total";
export type Counts = { exposed: number; converted: number };
export type Rate = {
  status: string;
  rate: number | null;
  confidence_interval: {
    method: string;
    level: number;
    lower: number;
    upper: number;
  } | null;
};
export type Comparison = {
  status: string;
  reason?: string;
  absolute_lift: number | null;
  relative_lift: number | null;
  relative_lift_reason?: string;
  z_statistic: number | null;
  p_value: number | null;
};
export type Results = {
  run_id: string;
  environment_id: string;
  control_variant_id: string;
  as_of: string;
  processing: {
    pending_events: number;
    due_users: number;
    oldest_pending_at: string | null;
    latest_reconciled_at: string | null;
    lag_seconds: number;
  };
  attribution_window_seconds: number;
  late_allowance_seconds: number;
  variants: Array<{
    id: string;
    weight_bp: number;
    provisional: Counts;
    finalized: Counts;
    total: Counts;
    provisional_rate: Rate;
    finalized_rate: Rate;
    total_rate: Rate;
    requests: {
      count: number;
      errors: number;
      error_rate: number | null;
      p95_upper_bound_ms: number | null;
      histogram: Array<{ upper_bound_ms: number; count: number }>;
    };
  }>;
  quality: {
    quarantined_events: number;
    future_events: number;
    pending_outcomes: number;
    invalid_reference_outcomes: number;
    outside_window_completions: number;
    duplicate_attributed_completions: number;
  };
  sample_ratio: Record<
    Cohort,
    {
      status: string;
      statistic: number | null;
      degrees_of_freedom: number;
      p_value: number | null;
      warning_threshold: number;
    }
  >;
  comparisons: Array<{
    control_variant_id: string;
    variant_id: string;
    provisional: Comparison;
    finalized: Comparison;
    total: Comparison;
  }>;
  inference_notes: string[];
};
export function basisPoints(text: string): number {
  if (!/^\d{1,3}(?:\.\d{1,2})?$/.test(text))
    throw new RangeError("Enter a percentage with at most two decimal places.");
  const [whole, fraction = ""] = text.split(".");
  const result = Number(whole) * 100 + Number(fraction.padEnd(2, "0"));
  if (result > 10000)
    throw new RangeError("The percentage cannot exceed 100%.");
  return result;
}
export function percent(value: number | null): string {
  return value === null ? "—" : `${(100 * value).toFixed(2)}%`;
}
export function label(value: string): string {
  return value.replaceAll("_", " ");
}
