"""Real local event recovery drill; preserves all queues and database volumes."""
import datetime
import json
import pathlib
import subprocess
import time
import urllib.error
import urllib.request
import uuid

from management_smoke import Client, base

COMPOSE = ["docker", "compose", "--profile", "cache", "--profile", "async"]


def command(args):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE).strip()


def wait_for(label, predicate):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.1)
    raise RuntimeError("Timed out: " + label)


def main():
    assert base in ("http://localhost:8080", "http://127.0.0.1:8080"), "Local stack required"
    admin = Client("admin@example.test")
    key = None
    broker_down = worker_down = False
    report = {"observed_at": datetime.datetime.now(datetime.timezone.utc).isoformat()}
    try:
        status, project = admin.call("POST", "/v1/projects", {"name": "Event drill " + uuid.uuid4().hex[:8]})
        assert status == 201
        project_id = project["id"]
        assert project_id.startswith("prj_") and project_id[4:].isalnum()
        path = "/v1/projects/" + project_id
        status, environments = admin.call("GET", path + "/environments")
        assert status == 200
        env = next(item["id"] for item in environments if item["name"] == "development")
        safe = {"type": "boolean", "data": False}
        assert admin.call("POST", path + "/flags", {"environment_id": env, "key": "listing", "type": "boolean", "default": safe, "safe": safe, "reason": "Event recovery baseline"})[0] == 201
        status, run = admin.call("POST", path + "/experiments", {
            "environment_id": env, "flag_key": "listing", "expected_revision": 1,
            "name": "Event recovery", "control_variant_id": "control", "traffic_bp": 10000,
            "variants": [{"id": "control", "ordinal": 0, "weight_bp": 5000, "value": safe},
                         {"id": "treatment", "ordinal": 1, "weight_bp": 5000, "value": {"type": "boolean", "data": True}}],
            "reason": "Verify durable processing"})
        assert status == 201
        assert admin.call("POST", path + "/experiments/" + run["id"] + "/transitions", {"action": "start", "expected_revision": 1, "reason": "Launch event recovery"})[0] == 200
        status, key = admin.call("POST", path + "/application-keys", {"name": "Local recovery", "environment_id": env, "permissions": ["evaluate", "events:write"]})
        assert status == 201

        def application(route, body, discard=False):
            request = urllib.request.Request(base + route, data=json.dumps(body).encode(), headers={"Authorization": "Bearer " + key["token"], "Content-Type": "application/json"})
            try:
                response = urllib.request.urlopen(request, timeout=5)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                if discard:
                    return response.status, None  # Deliberately drop the receipt body.
                return response.status, json.load(response)

        def events(user):
            status, decision = application("/v1/evaluate", {"project_id": project_id, "environment_id": env, "key": "listing", "user_id": user, "fallback": safe})
            assert status == 200 and decision["reason"] == "experiment"
            now = datetime.datetime.now(datetime.timezone.utc)
            exposure = {"event_id": user + "_exposure", "kind": "exposure", "run_id": run["id"], "user_id": user,
                        "variant_id": decision["variant_id"], "revision": decision["revision"], "decision_id": decision["decision_id"],
                        "decision_reason": "experiment", "occurred_at": (now - datetime.timedelta(minutes=2)).isoformat()}
            completion = {**exposure, "event_id": user + "_completion", "kind": "listing_completion", "exposure_id": exposure["event_id"], "occurred_at": (now - datetime.timedelta(minutes=1)).isoformat()}
            del completion["decision_id"]
            request = {**completion, "event_id": user + "_request", "kind": "request_outcome", "is_error": False, "latency_ms": 200}
            return [completion, request, exposure]

        def ingest(items, discard=False):
            return application("/v1/events", {"project_id": project_id, "environment_id": env, "events": items}, discard)

        def counts():
            query = f"""SELECT json_build_object(
              'raw',(SELECT count(*) FROM raw_events WHERE project_id='{project_id}'),
              'intents',(SELECT count(*) FROM outbox WHERE project_id='{project_id}'),
              'published',(SELECT count(*) FROM outbox WHERE project_id='{project_id}' AND published_at IS NOT NULL),
              'receipts',(SELECT count(*) FROM processed_work WHERE reference->>'project_id'='{project_id}'),
              'exposed',(SELECT COALESCE(sum(value),0) FROM metric_counts WHERE project_id='{project_id}' AND metric='exposed'),
              'converted',(SELECT COALESCE(sum(value),0) FROM metric_counts WHERE project_id='{project_id}' AND metric='converted'),
              'requests',(SELECT COALESCE(sum(value),0) FROM metric_counts WHERE project_id='{project_id}' AND category='request' AND metric='count'))"""
            return json.loads(command(COMPOSE + ["exec", "-T", "postgres", "psql", "-U", "switchyard", "-d", "switchyard", "-At", "-c", query]))

        def complete(n):
            state = counts()
            return state["raw"] == 3*n and state["published"] == state["intents"] == state["receipts"] and state["exposed"] == state["converted"] == state["requests"] == n

        wait_for("initial configuration receipts", lambda: complete(0))
        baseline = counts()
        first = events("recovery_one")
        broker_down = True
        command(COMPOSE + ["stop", "nats"])
        assert ingest(first, discard=True)[0] == 200, "Database acceptance failed during broker outage"
        during = counts()
        assert during["raw"] == 3 and during["published"] == baseline["published"] and during["receipts"] == baseline["receipts"], during
        report["broker_outage_counts"] = during
        command(COMPOSE + ["up", "--no-build", "-d", "--wait", "nats"])
        broker_down = False
        started = time.monotonic()
        wait_for("broker recovery processes acknowledged events", lambda: complete(1))
        report["broker_post_health_recovery_seconds"] = round(time.monotonic()-started, 4)
        status, retry = ingest(first)
        assert status == 200 and all(item["duplicate"] for item in retry["receipts"]), "Dropped receipt body changed retry identity"
        assert complete(1)

        second = events("recovery_two")
        worker_down = True
        command(COMPOSE + ["stop", "worker"])
        assert ingest(second)[0] == 200
        during = counts()
        assert during["raw"] == 6 and during["receipts"] == baseline["receipts"]+3 and during["exposed"] == 1, during
        report["worker_outage_counts"] = during
        command(COMPOSE + ["up", "--no-build", "-d", "--wait", "worker"])
        worker_down = False
        started = time.monotonic()
        wait_for("worker restart resumes durable event work", lambda: complete(2))
        report["worker_post_health_recovery_seconds"] = round(time.monotonic()-started, 4)
        stable = counts()
        worker_down = True
        command(COMPOSE + ["kill", "-s", "SIGKILL", "worker"])
        command(COMPOSE + ["up", "--no-build", "-d", "--wait", "worker"])
        worker_down = False
        wait_for("abrupt worker restart keeps committed metrics", lambda: complete(2))
        assert counts() == stable, "Abrupt restart changed receipt/counter identity"
        report["final_fixture_counts"] = stable
        report["parity"] = command(["make", "aggregation-parity"])
        report["scope"] = "Real local outages/restarts; convergence timings begin after service health/startup and exclude restart time; these are single observations, not throughput or percentiles. Receipt body was deliberately discarded; exact commit-before-broker-ack disconnect is covered by real PostgreSQL/JetStream integration."
    finally:
        if broker_down:
            command(COMPOSE + ["up", "--no-build", "-d", "--wait", "nats"])
        if worker_down:
            command(COMPOSE + ["up", "--no-build", "-d", "--wait", "worker"])
        if key is not None:
            admin.call("DELETE", path + "/application-keys/" + key["id"])
        admin.call("DELETE", "/v1/session")
    output = pathlib.Path(".cache/event-drill-report.json")
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
