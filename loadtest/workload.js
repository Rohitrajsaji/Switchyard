// k6 workloads for Switchyard. Open-model arrival-rate executors keep offering load when responses
// slow down, so saturation shows up as dropped iterations and latency rather than hidden queueing.
// WORKLOAD=evaluation|events|mixed. Configuration comes from environment variables set by
// scripts/load_run.py; see docs/benchmark-report.md for exact commands.
import http from "k6/http";
import exec from "k6/execution";
import { check } from "k6";
import { SharedArray } from "k6/data";
import { Counter, Rate } from "k6/metrics";

const fixture = JSON.parse(open(__ENV.FIXTURE || "/loadtest/fixture.json"));
const flags = new SharedArray("flags", () => fixture.flags);
const users = new SharedArray("users", () => fixture.users);
const base = __ENV.BASE_URL || "http://api:8080";
const workload = __ENV.WORKLOAD || "evaluation";
const nonce = __ENV.NONCE || "n0";
const headers = { Authorization: "Bearer " + fixture.token, "Content-Type": "application/json" };

const correct = new Rate("evaluation_correct");
const acceptedBatches = new Counter("event_batches_accepted");
const rejectedBatches = new Counter("event_batches_rejected");
const eventsAccepted = new Counter("events_accepted");

const scenarios = {};
if (workload !== "events") {
  scenarios.evaluate = {
    executor: "ramping-arrival-rate",
    exec: "evaluate",
    startRate: Number(__ENV.EVAL_START || 100),
    timeUnit: "1s",
    preAllocatedVUs: Number(__ENV.EVAL_VUS || 200),
    maxVUs: Number(__ENV.EVAL_MAX_VUS || 1000),
    stages: JSON.parse(__ENV.EVAL_STAGES || '[{"target":100,"duration":"30s"}]'),
  };
}
if (workload !== "evaluation") {
  scenarios.events = {
    executor: "constant-arrival-rate",
    exec: "ingest",
    rate: Number(__ENV.EVENT_BATCH_RATE || 10),
    timeUnit: "1s",
    duration: __ENV.EVENT_DURATION || "60s",
    preAllocatedVUs: Number(__ENV.EVENT_VUS || 50),
    maxVUs: Number(__ENV.EVENT_MAX_VUS || 200),
  };
}
export const options = {
  scenarios,
  summaryTrendStats: ["avg", "med", "p(90)", "p(95)", "p(99)", "max"],
  thresholds: {
    "http_req_failed{kind:evaluate}": ["rate<0.001"],
    "http_req_duration{kind:evaluate}": ["p(99)<50"],
    evaluation_correct: ["rate>0.999999"],
    "http_req_failed{kind:ingest}": ["rate<0.001"],
  },
};

// jsonb canonicalizes object key order, so compare JSON values structurally rather than as text.
const canonical = (v) =>
  Array.isArray(v) ? "[" + v.map(canonical).join(",") + "]" : v && typeof v === "object" ? "{" + Object.keys(v).sort().map((k) => JSON.stringify(k) + ":" + canonical(v[k])).join(",") + "}" : JSON.stringify(v);
const countries = ["JP", "US", "FR", "BR"];
const pick = (list, n) => list[n % list.length];

export function evaluate() {
  const n = exec.scenario.iterationInTest;
  const flag = flags[n % flags.length];
  const user = "eval-user-" + ((n * 7919) % 100000);
  const country = pick(countries, n >> 3);
  const age = 10 + ((n >> 5) % 50);
  const safe = flag.kind === "json" ? flag.safe : { type: "boolean", data: false };
  const response = http.post(
    base + "/v1/evaluate",
    JSON.stringify({
      project_id: fixture.project_id,
      environment_id: fixture.environment_id,
      key: flag.key,
      user_id: user,
      attributes: { country, age, tier: "standard" },
      fallback: safe,
    }),
    { headers, tags: { kind: "evaluate" } },
  );
  let ok = response.status === 200;
  if (ok) {
    const body = response.json();
    switch (flag.kind) {
      case "always_on":
        ok = body.value.data === true && body.reason === "default";
        break;
      case "rules_country":
        ok = country === "JP" ? body.value.data === true && body.reason === "targeting" : body.value.data === false && body.reason === "default";
        break;
      case "rules_age":
        ok = age >= 18 ? body.value.data === true && body.reason === "targeting" : body.value.data === false && body.reason === "default";
        break;
      case "json":
        ok = canonical(body.value.data) === canonical(flag.expected) && body.value.type === "json";
        break;
      case "killed":
        ok = body.value.data === false && body.reason === "kill_switch";
        break;
      case "rollout":
        ok = body.reason === "rollout" ? body.value.data === true : body.reason === "default" && body.value.data === false;
        break;
      case "experiment":
        ok = body.reason === "experiment" && body.run_id === flag.run_id && (body.variant_id === "control" ? body.value.data === false : body.variant_id === "treatment" && body.value.data === true);
        break;
      default:
        ok = false;
    }
    ok = ok && typeof body.decision_id === "string" && body.decision_id.length > 0;
  }
  correct.add(ok);
}

// Each batch carries 33 users x (exposure, completion, request outcome) = 99 events.
export function ingest() {
  const n = exec.scenario.iterationInTest;
  const now = Date.now();
  const events = [];
  for (let j = 0; j < 33; j++) {
    const [user, variant] = users[(n * 33 + j) % users.length];
    const id = `${nonce}-${n}-${j}`;
    const common = { run_id: fixture.run_id, user_id: user, variant_id: variant, revision: fixture.revision, decision_reason: "experiment" };
    events.push({ ...common, event_id: "x" + id, kind: "exposure", decision_id: "dec_" + id, occurred_at: new Date(now - 2000).toISOString() });
    events.push({ ...common, event_id: "c" + id, kind: "listing_completion", exposure_id: "x" + id, occurred_at: new Date(now - 1000).toISOString() });
    events.push({ ...common, event_id: "r" + id, kind: "request_outcome", exposure_id: "x" + id, occurred_at: new Date(now - 1000).toISOString(), is_error: false, latency_ms: 120 });
  }
  const response = http.post(
    base + "/v1/events",
    JSON.stringify({ project_id: fixture.project_id, environment_id: fixture.environment_id, events }),
    { headers, tags: { kind: "ingest" } },
  );
  const accepted =
    response.status === 200 &&
    check(response, { "all receipts accepted": (r) => r.json("receipts").every((x) => x.status === "accepted") });
  if (accepted) {
    acceptedBatches.add(1);
    eventsAccepted.add(events.length);
  } else {
    rejectedBatches.add(1);
  }
}
