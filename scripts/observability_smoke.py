"""Observability smoke: metrics, bounded labels, provisioned Grafana and an end-to-end trace.

Requires `make observability-up` (API and worker started with OTLP export at full sampling) and
seeded demo credentials. Everything it creates is synthetic."""
import base64
import datetime
import json
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from management_smoke import Client, base

PROMETHEUS = "http://127.0.0.1:9091"
TEMPO = "http://127.0.0.1:3200"
GRAFANA = "http://127.0.0.1:3001"
SAFE = {"type": "boolean", "data": False}
ON = {"type": "boolean", "data": True}


def get(url, headers=None, timeout=10):
    request = urllib.request.Request(url, headers=headers or {})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.load(response)


def prom(query):
    return get(PROMETHEUS + "/api/v1/query?" + urllib.parse.urlencode({"query": query}))["data"]["result"]


def wait(description, check, seconds=60):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (urllib.error.URLError, KeyError, IndexError, ValueError):
            pass
        time.sleep(1)
    raise AssertionError("timed out: " + description)


def main():
    admin = Client("admin@example.test")
    status, project = admin.call("POST", "/v1/projects", {"name": "Observe " + uuid.uuid4().hex[:8]})
    assert status == 201
    path = "/v1/projects/" + project["id"]
    environments = admin.call("GET", path + "/environments")[1]
    dev = next(e["id"] for e in environments if e["name"] == "development")
    assert admin.call("POST", path + "/flags", {"environment_id": dev, "key": "listing", "type": "boolean",
        "default": SAFE, "safe": SAFE, "reason": "observability smoke"})[0] == 201
    status, run = admin.call("POST", path + "/experiments", {"environment_id": dev, "flag_key": "listing",
        "expected_revision": 1, "name": "Observe", "control_variant_id": "control", "traffic_bp": 10000,
        "variants": [{"id": "control", "ordinal": 0, "weight_bp": 5000, "value": SAFE},
                     {"id": "treatment", "ordinal": 1, "weight_bp": 5000, "value": ON}], "reason": "observe"})
    assert status == 201
    assert admin.call("POST", f"{path}/experiments/{run['id']}/transitions",
                      {"action": "start", "expected_revision": 1, "reason": "observe"})[0] == 200
    status, key = admin.call("POST", path + "/application-keys", {
        "name": "Observe", "environment_id": dev, "permissions": ["evaluate", "events:write"]})
    assert status == 201

    def application(route, data):
        request = urllib.request.Request(base + route, data=json.dumps(data).encode(), headers={
            "Authorization": "Bearer " + key["token"], "Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=10) as response:
            return json.load(response)

    user_ids = [f"observe-user-{uuid.uuid4().hex[:10]}" for _ in range(40)]
    decisions = [application("/v1/evaluate", {"project_id": project["id"], "environment_id": dev, "key": "listing",
        "user_id": u, "fallback": SAFE}) for u in user_ids]
    event_ids = []
    events = []
    now = datetime.datetime.now(datetime.UTC).isoformat()
    for user, decision in zip(user_ids, decisions):
        event_id = "exposure-" + uuid.uuid4().hex
        event_ids.append(event_id)
        events.append({"event_id": event_id, "kind": "exposure", "run_id": run["id"], "user_id": user,
            "variant_id": decision["variant_id"], "revision": decision["revision"], "decision_id": decision["decision_id"],
            "decision_reason": "experiment", "occurred_at": now})
    receipts = application("/v1/events", {"project_id": project["id"], "environment_id": dev, "events": events})["receipts"]
    assert all(r["status"] == "accepted" for r in receipts)
    print("traffic generated: 40 evaluations, 40 event receipts")

    # Prometheus scrapes both services.
    wait("both services scraped", lambda: len(prom('up{job=~"switchyard-.*"} == 1')) == 2)
    wait("evaluation route metric", lambda: float(prom('sum(switchyard_http_requests_total{route="POST /v1/evaluate",class="2xx"})')[0]["value"][1]) >= 40)
    wait("latency histogram", lambda: prom('histogram_quantile(0.99, sum by (le)(rate(switchyard_http_request_duration_seconds_bucket{route="POST /v1/evaluate"}[1m])))'))
    assert prom('sum(switchyard_evaluations_total{reason="experiment"})')
    assert prom("switchyard_cache_oldest_verification_age_seconds")
    assert prom("switchyard_db_pool_max_connections")
    assert prom("process_resident_memory_bytes")
    wait("events counted", lambda: float(prom('sum(switchyard_events_total{outcome="accepted"})')[0]["value"][1]) >= 40)
    wait("worker consumed events", lambda: float(prom('sum(switchyard_work_messages_total{result="committed"})')[0]["value"][1]) >= 40)
    wait("outbox drained", lambda: prom("switchyard_outbox_unpublished") and float(prom("max(switchyard_outbox_unpublished)")[0]["value"][1]) == 0)
    assert prom("switchyard_consumer_pending_messages"), "consumer lag metric missing"
    print("prometheus: scrape health, route latency, cache staleness, events, consumer lag, resources present")

    # No user ID, event ID or decision ID may appear in any label value.
    series = get(PROMETHEUS + "/api/v1/series?" + urllib.parse.urlencode({"match[]": '{__name__=~"switchyard_.*"}'}))["data"]
    haystack = json.dumps(series)
    for secret in user_ids + event_ids + [d["decision_id"] for d in decisions] + [project["id"], run["id"], dev]:
        assert secret not in haystack, "unbounded identifier became a label"
    labels = {k for s in series for k in s}
    assert labels <= {"__name__", "service", "route", "method", "class", "code", "reason", "outcome", "result", "le", "job", "instance"}, labels
    print(f"label audit: {len(series)} series, labels {sorted(labels - {'__name__'})}")

    # Grafana is provisioned.
    auth = {"Authorization": "Basic " + base64.b64encode(b"admin:switchyard-local-only").decode()}
    wait("grafana healthy", lambda: get(GRAFANA + "/api/health")["database"] == "ok")
    dashboard = get(GRAFANA + "/api/dashboards/uid/switchyard-overview", auth)["dashboard"]
    assert len(dashboard["panels"]) >= 15
    for uid in ("prometheus", "tempo"):
        assert get(f"{GRAFANA}/api/datasources/uid/{uid}/health", auth)["status"] == "OK"
    print(f"grafana: dashboard with {len(dashboard['panels'])} panels, prometheus and tempo datasources healthy")

    # One trace follows an accepted event through ingestion, publication and processing.
    wanted = {"POST /v1/events", "db.insert", "outbox.publish", "processing.consume"}

    def find_trace():
        query = '{ name = "POST /v1/events" && resource.service.name = "switchyard-api" }'
        search = {"q": query, "limit": 5, "start": int(time.time()) - 600, "end": int(time.time()) + 60}
        for found in get(TEMPO + "/api/search?" + urllib.parse.urlencode(search)).get("traces", []):
            text = json.dumps(get(TEMPO + "/api/v2/traces/" + found["traceID"], {"Accept": "application/json"}))
            if all(f'"{name}"' in text for name in wanted):
                return found["traceID"], text
        return None

    trace_id, text = wait("end-to-end trace", find_trace, 90)
    assert all(service in text for service in ("switchyard-api", "switchyard-worker"))
    for secret in user_ids[:5] + event_ids[:5]:
        assert secret not in text, "identifier recorded in a span"
    assert "FROM " not in text and "INSERT INTO" not in text, "SQL text in span"
    print(f"tempo: trace {trace_id} spans api request, database writes, outbox publication and worker consumption")
    print("observability smoke passed")
    assert admin.call("DELETE", "/v1/session")[0] == 204


if __name__ == "__main__":
    main()
