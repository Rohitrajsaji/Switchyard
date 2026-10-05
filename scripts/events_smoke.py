"""Live explicit event journey; requires opt-in seeded demo credentials."""
import datetime
import json
import time
import urllib.error
import urllib.request
import uuid

from management_smoke import Client, base


def main():
    admin = Client("admin@example.test")
    status, project = admin.call("POST", "/v1/projects", {"name": "Events " + uuid.uuid4().hex[:8]})
    assert status == 201
    path = "/v1/projects/" + project["id"]
    status, environments = admin.call("GET", path + "/environments")
    assert status == 200
    env = {item["name"]: item["id"] for item in environments}
    safe = {"type": "boolean", "data": False}
    treatment = {"type": "boolean", "data": True}
    status, _ = admin.call("POST", path + "/flags", {
        "environment_id": env["development"], "key": "listing", "type": "boolean",
        "default": safe, "safe": safe, "reason": "event baseline",
        "rules": [{"attribute": "country", "operator": "eq", "values": ["JP"], "value": treatment}],
    })
    assert status == 201
    status, run = admin.call("POST", path + "/experiments", {
        "environment_id": env["development"], "flag_key": "listing", "expected_revision": 1,
        "name": "Listing events", "control_variant_id": "control", "traffic_bp": 10000,
        "variants": [
            {"id": "control", "ordinal": 0, "weight_bp": 5000, "value": safe},
            {"id": "treatment", "ordinal": 1, "weight_bp": 5000, "value": treatment},
        ], "reason": "measure explicit outcomes",
    })
    assert status == 201
    assert admin.call("POST", path + "/experiments/" + run["id"] + "/transitions", {
        "action": "start", "expected_revision": 1, "reason": "launch event smoke"
    })[0] == 200
    status, key = admin.call("POST", path + "/application-keys", {
        "name": "Event smoke", "environment_id": env["development"], "permissions": ["evaluate", "events:write"]
    })
    assert status == 201

    def application(route, data):
        request = urllib.request.Request(base + route, data=json.dumps(data).encode(), headers={
            "Authorization": "Bearer " + key["token"], "Content-Type": "application/json"
        })
        try:
            response = urllib.request.urlopen(request, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, json.load(response)

    def ingest(items, environment=None):
        return application("/v1/events", {"project_id": project["id"],
            "environment_id": environment or env["development"], "events": items})

    def results():
        deadline = time.monotonic() + 30
        while True:
            status, data = admin.call("GET", path + "/experiments/" + run["id"] + "/results")
            assert status == 200 and data["run_id"] == run["id"]
            processing = data["processing"]
            if processing["pending_events"] == 0 and processing["due_users"] == 0:
                return data
            assert time.monotonic() < deadline, "Asynchronous measurement did not converge"
            time.sleep(0.2)

    status, decision = application("/v1/evaluate", {"project_id": project["id"],
        "environment_id": env["development"], "key": "listing", "user_id": "synthetic-user", "fallback": safe})
    assert status == 200 and decision["reason"] == "experiment"
    now = datetime.datetime.now(datetime.UTC)
    exposure = {"event_id": "exposure_1", "kind": "exposure", "run_id": run["id"],
        "user_id": "synthetic-user", "variant_id": decision["variant_id"], "revision": decision["revision"],
        "decision_id": decision["decision_id"], "decision_reason": "experiment",
        "occurred_at": (now - datetime.timedelta(minutes=2)).isoformat()}
    completion = {**exposure, "event_id": "completion_1", "kind": "listing_completion",
        "exposure_id": exposure["event_id"], "occurred_at": (now - datetime.timedelta(minutes=1)).isoformat()}
    del completion["decision_id"]
    request = {**completion, "event_id": "request_1", "kind": "request_outcome", "is_error": False, "latency_ms": 200}
    # Outcomes arrive first; acceptance is durable fact storage, not yet attribution.
    for items in [[completion, request], [exposure]]:
        status, response = ingest(items)
        assert status == 200 and all(item["status"] == "accepted" and not item["duplicate"] for item in response["receipts"])
        measured = results()
        if items[0]["kind"] == "listing_completion":
            assert measured["quality"]["pending_outcomes"] == 2
            assert sum(v["total"]["exposed"] for v in measured["variants"]) == 0
        else:
            assert measured["quality"]["pending_outcomes"] == 0
            counted = next(v for v in measured["variants"] if v["id"] == decision["variant_id"])
            assert counted["provisional"] == {"exposed": 1, "converted": 1}
            assert counted["finalized"] == {"exposed": 0, "converted": 0}
            assert counted["requests"]["count"] == 1 and counted["requests"]["p95_upper_bound_ms"] == 250
            assert counted["total_rate"]["confidence_interval"]["method"] == "wilson"
    status, response = ingest([request, exposure, completion])
    assert status == 200 and all(item["duplicate"] for item in response["receipts"])
    assert ingest([{**completion, "event_id": "completion_new_id"}])[0] == 200
    measured = results()
    assert sum(v["total"]["converted"] for v in measured["variants"]) == 1
    assert measured["quality"]["duplicate_attributed_completions"] == 1
    changed = {**exposure, "user_id": "changed-user"}
    atomic = {**exposure, "event_id": "a_atomic"}
    assert ingest([atomic, changed])[0] == 409
    status, response = ingest([atomic])
    assert status == 200 and response["receipts"][0]["duplicate"] is False
    quarantined = [
        {**exposure, "event_id": "wrong_variant", "variant_id": "invalid"},
        {**exposure, "event_id": "late", "occurred_at": (now - datetime.timedelta(hours=25)).isoformat()},
        {**exposure, "event_id": "targeted", "attributes": {"country": "JP"}, "decision_reason": "targeting", "variant_id": ""},
    ]
    status, response = ingest(quarantined)
    assert status == 200
    assert [item["reason"] for item in response["receipts"]] == ["assignment_mismatch", "too_late", "non_randomized_exposure"]
    assert all(item["status"] == "quarantined" for item in response["receipts"])
    measured = results()
    assert measured["quality"]["quarantined_events"] == 3
    assert sum(v["total"]["exposed"] for v in measured["variants"]) == 1
    assert measured["comparisons"][0]["total"]["status"] == "insufficient_data"
    assert ingest([exposure], env["staging"])[0] == 403
    assert ingest([exposure] * 101)[0] == 400
    assert admin.call("DELETE", path + "/application-keys/" + key["id"])[0] == 204
    assert ingest([exposure])[0] == 401
    assert admin.call("DELETE", "/v1/session")[0] == 204
    print("Live measurement passed: pending reconciliation, unique conversion, Wilson interval, product histogram, provisional labels, conflict and quarantine")


if __name__ == "__main__":
    main()
