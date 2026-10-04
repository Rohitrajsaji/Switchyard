import type { Decision, Value } from "./flags";
export type MeasurementEvent = {
  event_id: string;
  kind: "exposure" | "listing_completion" | "request_outcome";
  run_id: string;
  user_id: string;
  variant_id: string;
  revision: number;
  decision_reason: string;
  occurred_at: string;
  attributes: unknown;
  decision_id?: string;
  exposure_id?: string;
  is_error?: boolean;
  latency_ms?: number;
};
export type Receipt = {
  event_id: string;
  status: "accepted" | "quarantined";
  reason?: string;
  duplicate: boolean;
};
export function listingFlow(value: Value): "classic" | "simple" {
  if (value.type === "boolean" && typeof value.data === "boolean")
    return value.data ? "simple" : "classic";
  if (
    value.type === "json" &&
    value.data &&
    typeof value.data === "object" &&
    "flow" in value.data &&
    (value.data.flow === "classic" || value.data.flow === "simple")
  )
    return value.data.flow;
  throw new Error(
    'The listing demo requires a boolean value or JSON with flow: "classic" or "simple".',
  );
}
export function exposureEvent(
  decision: Decision,
  userID: string,
  attributes: unknown,
  id: string,
  time: string,
): MeasurementEvent {
  if (
    decision.reason !== "experiment" ||
    !decision.run_id ||
    !decision.variant_id
  )
    throw new Error("Only randomized decisions are measured in this demo.");
  return {
    event_id: id,
    kind: "exposure",
    run_id: decision.run_id,
    user_id: userID,
    variant_id: decision.variant_id,
    revision: decision.revision,
    decision_reason: decision.reason,
    decision_id: decision.decision_id,
    attributes,
    occurred_at: time,
  };
}
export function outcomeEvent(
  exposure: MeasurementEvent,
  id: string,
  time: string,
  error: boolean,
  latency: number,
): MeasurementEvent {
  const { decision_id: _, event_id: exposureID, ...context } = exposure;
  return {
    ...context,
    event_id: id,
    kind: "request_outcome",
    exposure_id: exposureID,
    occurred_at: time,
    is_error: error,
    latency_ms: latency,
  };
}
export function completionEvent(
  exposure: MeasurementEvent,
  id: string,
  time: string,
): MeasurementEvent {
  const { decision_id: _, event_id: exposureID, ...context } = exposure;
  return {
    ...context,
    event_id: id,
    kind: "listing_completion",
    exposure_id: exposureID,
    occurred_at: time,
  };
}
export function checkReceipts(
  events: MeasurementEvent[],
  receipts: Receipt[],
): void {
  if (
    events.length !== receipts.length ||
    events.some((event, index) => receipts[index].event_id !== event.event_id)
  )
    throw new Error("Event acknowledgement did not match the submitted batch.");
  const quarantine = receipts.find((r) => r.status !== "accepted");
  if (quarantine)
    throw new Error(
      `Event quarantined: ${quarantine.reason ?? "invalid measurement"}. It is excluded from experiment results.`,
    );
}
