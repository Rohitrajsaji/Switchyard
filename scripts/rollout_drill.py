"""Live rollout drill: approved metric-driven promotion, then a deliberately broken treatment that
triggers the automatic safety rollback, with measured propagation to evaluation.

Requires the Docker stack with the worker running and seeded demo credentials. Uses the plan's
default guardrails (1,000 eligible requests per evaluated variant, two consecutive breaches)."""
import concurrent.futures
import datetime
import json
import time
import urllib.error
import urllib.request
import uuid

from management_smoke import Client, base

SAFE = {"type": "boolean", "data": False}
ON = {"type": "boolean", "data": True}


def parse(ts):
    return datetime.datetime.fromisoformat(ts.replace("Z", "+00:00"))


def main():
    admin = Client("admin@example.test")
    reviewer = Client("reviewer@example.test")
    developer = Client("developer@example.test")
    status, project = admin.call("POST", "/v1/projects", {"name": "Rollout " + uuid.uuid4().hex[:8]})
    assert status == 201
    path = "/v1/projects/" + project["id"]
    for user in ("demo_reviewer", "demo_developer"):
        assert admin.call("POST", path + "/members", {"user_id": user})[0] == 204
    status, environments = admin.call("GET", path + "/environments")
    dev = next(e["id"] for e in environments if e["name"] == "development")
    assert admin.call("POST", path + "/flags", {"environment_id": dev, "key": "listing_flow", "type": "boolean",
        "default": SAFE, "safe": SAFE, "reason": "rollout drill"})[0] == 201
    status, run = admin.call("POST", path + "/experiments", {
        "environment_id": dev, "flag_key": "listing_flow", "expected_revision": 1, "name": "Rollout drill",
        "control_variant_id": "control", "traffic_bp": 8000,
        "variants": [{"id": "control", "ordinal": 0, "weight_bp": 5000, "value": SAFE},
                     {"id": "treatment", "ordinal": 1, "weight_bp": 5000, "value": ON}], "reason": "rollout drill"})
    assert status == 201
    status, started = admin.call("POST", f"{path}/experiments/{run['id']}/transitions",
                                 {"action": "start", "expected_revision": 1, "reason": "rollout drill"})
    assert status == 200
    status, key = admin.call("POST", path + "/application-keys", {
        "name": "Rollout drill", "environment_id": dev, "permissions": ["evaluate", "events:write"]})
    assert status == 201

    def application(route, data):
        request = urllib.request.Request(base + route, data=json.dumps(data).encode(), headers={
            "Authorization": "Bearer " + key["token"], "Content-Type": "application/json"})
        try:
            response = urllib.request.urlopen(request, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, json.load(response)

    def evaluate(user):
        status, decision = application("/v1/evaluate", {"project_id": project["id"], "environment_id": dev,
            "key": "listing_flow", "user_id": user, "fallback": SAFE})
        assert status == 200, decision
        return user, decision

    print("collecting 1,000 treatment-assigned users ...")
    treatment, i = [], 0
    with concurrent.futures.ThreadPoolExecutor(8) as pool:
        while len(treatment) < 1000:
            batch = [f"drill-user-{i + n}" for n in range(400)]
            i += 400
            for user, decision in pool.map(evaluate, batch):
                if decision["reason"] == "experiment" and decision["variant_id"] == "treatment":
                    treatment.append((user, decision))
    treatment = treatment[:1000]

    def send(users, errors, latency):
        now = datetime.datetime.now(datetime.UTC).isoformat()
        events = []
        for n, (user, decision) in enumerate(users):
            events.append({"event_id": "req-" + uuid.uuid4().hex, "kind": "request_outcome", "run_id": run["id"],
                "user_id": user, "variant_id": "treatment", "revision": decision["revision"],
                "decision_reason": "experiment", "exposure_id": "exposure-" + user, "occurred_at": now,
                "is_error": n < errors, "latency_ms": latency})
        for start in range(0, len(events), 100):
            status, body = application("/v1/events", {"project_id": project["id"], "environment_id": dev,
                "events": events[start:start + 100]})
            assert status == 200 and all(r["status"] == "accepted" for r in body["receipts"]), body

    def flag():
        status, body = admin.call("GET", f"{path}/flags/listing_flow?environment_id={dev}")
        assert status == 200
        return body

    def plan_state(plan_id):
        status, body = admin.call("GET", f"{path}/rollouts/{plan_id}")
        assert status == 200
        return body

    def wait_state(plan_id, states, seconds):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            plan = plan_state(plan_id)
            if plan["state"] in states:
                return plan
            time.sleep(1)
        raise AssertionError("plan did not reach " + str(states) + ": " + plan_state(plan_id)["state"])

    def create_plan(mode, ceiling, steps):
        status, plan = developer.call("POST", path + "/rollouts", {
            "environment_id": dev, "run_id": run["id"], "mode": mode, "ceiling_bp": ceiling, "steps": steps,
            "rationale": "rollout drill " + mode})
        assert status == 201 and plan["state"] == "proposed", plan
        return plan

    # Bounds and separation of duties, over HTTP.
    assert developer.call("POST", path + "/rollouts", {"environment_id": dev, "run_id": run["id"], "mode": "metric",
        "ceiling_bp": 10000, "steps": [{"traffic_bp": 9500, "offset_seconds": 5}], "rationale": "too big"})[0] == 400
    assert developer.call("POST", f"{path}/experiments/{run['id']}/traffic", {"expected_revision": started["configuration_revision"], "traffic_bp": 9500, "reason": "manual jump"})[0] == 400

    # Phase 1: healthy treatment promotes under an approved metric-driven plan.
    plan = create_plan("metric", 9000, [{"traffic_bp": 9000, "offset_seconds": 5}])
    assert developer.call("POST", f"{path}/rollouts/{plan['id']}/approve", {"plan_hash": plan["plan_hash"], "reason": "self"})[0] == 403
    assert developer.call("POST", f"{path}/rollouts/{plan['id']}/start")[0] == 409  # not approved
    assert reviewer.call("POST", f"{path}/rollouts/{plan['id']}/approve", {"plan_hash": plan["plan_hash"], "reason": "reviewed"})[0] == 200
    send(treatment, 0, 100)
    assert developer.call("POST", f"{path}/rollouts/{plan['id']}/start")[0] == 200
    promoted = wait_state(plan["id"], {"completed", "rolled_back", "stale", "cancelled"}, 90)
    assert promoted["state"] == "completed" and promoted["steps"][0]["state"] == "applied", promoted
    assert flag()["experiment"]["traffic_bp"] == 9000 and not flag()["killed"]
    status, checks = admin.call("GET", f"{path}/rollouts/{plan['id']}/checks")
    assert status == 200 and checks[0]["decision"] == "pass", checks
    print("phase 1: healthy metric-driven step applied; traffic 8000 -> 9000")

    # Phase 2: the treatment breaks; two consecutive breaches trigger the safety rollback.
    plan = create_plan("scheduled", 10000, [{"traffic_bp": 10000, "offset_seconds": 600}])
    assert reviewer.call("POST", f"{path}/rollouts/{plan['id']}/approve", {"plan_hash": plan["plan_hash"], "reason": "reviewed"})[0] == 200
    assert developer.call("POST", f"{path}/rollouts/{plan['id']}/start")[0] == 200
    send(treatment, 300, 900)
    broken_at = time.time()
    rolled = wait_state(plan["id"], {"rolled_back", "completed", "stale", "cancelled"}, 90)
    assert rolled["state"] == "rolled_back" and rolled["steps"][0]["state"] == "cancelled", rolled
    detection = time.time() - broken_at
    assert flag()["killed"] is True
    status, checks = admin.call("GET", f"{path}/rollouts/{plan['id']}/checks")
    assert sum(c["decision"] == "breach" for c in checks) >= 2, checks

    # Bounded propagation to evaluation: a treatment user must receive the safe value.
    user = treatment[0][0]
    finished = parse(rolled["finished_at"]).timestamp()
    deadline = time.time() + 30
    while True:
        _, decision = evaluate(user)
        if decision["reason"] == "kill_switch":
            propagation = time.time() - finished
            break
        assert time.time() < deadline, "kill switch did not propagate within 30 seconds"
        time.sleep(0.1)
    assert decision["value"] == SAFE

    status, entries = admin.call("GET", path + "/audit")
    rollback = next(e for e in entries if e["action"] == "rollout.rolled_back")
    assert rollback["actor_id"] == "system:rollout" and rollback["details"]["evidence"]["operational"][0]["breaches"], rollback
    assert any(e["action"] == "flag.traffic_changed" and e["details"].get("plan_id") for e in entries)
    print(f"phase 2: automatic rollback after {detection:.1f}s from broken traffic; "
          f"evaluation returned the safe value {propagation:.2f}s after the rollback committed")
    print("rollout drill passed: bounded approval, metric promotion, breach rollback, cancellation, evidence, propagation")
    assert admin.call("DELETE", "/v1/session")[0] == 204


if __name__ == "__main__":
    main()
