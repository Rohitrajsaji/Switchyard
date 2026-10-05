"""Bounded local open-loop ingestion trial. Never deletes data or retries load requests."""
import argparse
import concurrent.futures
import datetime
import hashlib
import json
import math
import pathlib
import struct
import subprocess
import threading
import time
import urllib.error
import urllib.request
import uuid

from management_smoke import Client, base

COMPOSE = ["docker", "compose", "--profile", "cache", "--profile", "async"]


def command(args):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE, timeout=30).strip()


def percentile(values, fraction):
    ordered = sorted(values)
    return ordered[max(0, math.ceil(len(ordered) * fraction) - 1)] if ordered else None


def sql(query):
    return json.loads(command(COMPOSE + ["exec", "-T", "postgres", "psql", "-U", "switchyard", "-d", "switchyard", "-At", "-c", query]))


def freshness(project_id):
    return sql(f"""WITH per_user AS (
      SELECT user_id,max(received_at) received FROM raw_events WHERE project_id='{project_id}' GROUP BY user_id),
      delays AS (SELECT greatest(0,extract(epoch FROM s.reconciled_at-p.received)) delay
      FROM per_user p JOIN metric_user_state s USING(user_id) WHERE s.project_id='{project_id}')
      SELECT json_build_object('users',count(*),'p50_seconds',percentile_cont(.5) WITHIN GROUP(ORDER BY delay),
      'p95_seconds',percentile_cont(.95) WITHIN GROUP(ORDER BY delay),'max_seconds',max(delay)) FROM delays""")


def finish_existing(filename):
    """Verify eventual convergence after a recorded timeout without repeating load."""
    source = pathlib.Path(filename)
    report = json.loads(source.read_text())
    project_id = report["project_id"]
    run_id = report["run_id"]
    assert project_id.startswith("prj_") and project_id[4:].isalnum()
    assert run_id.startswith("run_") and run_id[4:].isalnum()
    expected = report["validated_http_accepted_events"]
    deadline = time.monotonic()+180
    while True:
        final = sql(f"""SELECT json_build_object(
          'raw',(SELECT count(*) FROM raw_events WHERE project_id='{project_id}'),
          'quarantined',(SELECT count(*) FROM raw_events WHERE project_id='{project_id}' AND status<>'accepted'),
          'intents',(SELECT count(*) FROM outbox WHERE project_id='{project_id}'),
          'published',(SELECT count(*) FROM outbox WHERE project_id='{project_id}' AND published_at IS NOT NULL),
          'receipts',(SELECT count(*) FROM processed_work WHERE reference->>'project_id'='{project_id}'),
          'due_users',(SELECT count(*) FROM metric_user_state WHERE project_id='{project_id}' AND due_at<=clock_timestamp()),
          'exposed',(SELECT COALESCE(sum(value),0) FROM metric_counts WHERE project_id='{project_id}' AND metric='exposed' AND category='provisional'),
          'converted',(SELECT COALESCE(sum(value),0) FROM metric_counts WHERE project_id='{project_id}' AND metric='converted' AND category='provisional'),
          'requests',(SELECT COALESCE(sum(value),0) FROM metric_counts WHERE project_id='{project_id}' AND category='request' AND metric='count'))""")
        if final["intents"] == final["published"] == final["receipts"] and final["due_users"] == 0:
            break
        assert time.monotonic() < deadline, "Follow-up observation also timed out"
        print("Follow-up convergence", json.dumps(final), flush=True)
        time.sleep(5)
    assert final["raw"] == expected and final["quarantined"] == 0
    assert final["exposed"] == final["converted"] == final["requests"] == expected//3
    report["follow_up_observed_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    report["drain_timeout_seconds"] = 180
    report["final_counts"] = final
    report["database_freshness"] = freshness(project_id)
    report["parity_output"] = command(["make", "aggregation-parity"])
    # Reuse the persisted semantic payloads; no fresh event IDs or timestamp changes.
    sample = sql(f"""SELECT json_build_object('environment_id',min(environment_id),'events',json_agg(payload ORDER BY event_id))
      FROM raw_events WHERE project_id='{project_id}' AND user_id='trial_0'""")
    assert len(sample["events"]) == 3
    admin = Client("admin@example.test")
    key = None
    path = "/v1/projects/" + project_id
    try:
        status, key = admin.call("POST", path + "/application-keys", {"name": "Trial follow-up retry", "environment_id": sample["environment_id"], "permissions": ["events:write"]})
        assert status == 201
        request = urllib.request.Request(base + "/v1/events", data=json.dumps({"project_id": project_id, **sample}).encode(),
                                         headers={"Authorization": "Bearer " + key["token"], "Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=10) as response:
            receipts = json.load(response)["receipts"]
        assert len(receipts) == 3 and all(item["duplicate"] and item["status"] == "accepted" for item in receipts)
        after = sql(f"SELECT json_build_object('raw',(SELECT count(*) FROM raw_events WHERE project_id='{project_id}'),'intents',(SELECT count(*) FROM outbox WHERE project_id='{project_id}'))")
        assert after["raw"] == final["raw"] and after["intents"] == final["intents"]
        report["duplicate_retry_verified"] = True
        report["passed_eventual_correctness"] = True
    finally:
        if key is not None:
            assert admin.call("DELETE", path + "/application-keys/" + key["id"])[0] == 204
        assert admin.call("DELETE", "/v1/session")[0] == 204
    destination = source.with_name("ingestion-trial-follow-up.json")
    destination.write_text(json.dumps(report, indent=2)+"\n")
    print(json.dumps({key: value for key, value in report.items() if key not in ("resource_samples", "queue_samples")}, indent=2), flush=True)


def bucket(definition, user):
    experiment = definition["experiment"]
    fields = ["v1", definition["project_id"], definition["environment_id"],
              definition["flag_id"], experiment["run_id"], "variant", experiment["variant_salt"], user]
    encoded = b"".join(struct.pack(">I", len(item.encode())) + item.encode() for item in fields)
    value = int.from_bytes(hashlib.sha256(encoded).digest()[:8], "big") % 10000
    end = 0
    for variant in sorted(experiment["variants"], key=lambda item: item["ordinal"]):
        end += variant["weight_bp"]
        if value < end:
            return variant["id"]
    raise ValueError("Invalid allocation")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rate", type=int, default=1000)
    parser.add_argument("--seconds", type=int, default=30)
    parser.add_argument("--finish-report", help="Observe an existing trial; do not submit new load")
    args = parser.parse_args()
    assert base in ("http://localhost:8080", "http://127.0.0.1:8080"), "Local stack required"
    if args.finish_report:
        finish_existing(args.finish_report)
        return
    assert 1 <= args.rate <= 10000 and 1 <= args.seconds <= 120, "Bounded trial required"
    assert args.rate * args.seconds >= 99, "Trial must offer at least one complete 99-event batch"
    admin = Client("admin@example.test")
    key = None
    stop = threading.Event()
    observer = None
    report = {"observed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "requested_events_per_second": args.rate, "offering_seconds": args.seconds,
              "batch_events": 99, "request_workers": 16, "maximum_inflight_batches": 32,
              "resource_samples": [], "queue_samples": [], "observer_errors": []}
    results = []
    try:
        status, project = admin.call("POST", "/v1/projects", {"name": "Ingestion trial " + uuid.uuid4().hex[:8]})
        assert status == 201
        project_id = project["id"]
        assert project_id.startswith("prj_") and project_id[4:].isalnum()
        path = "/v1/projects/" + project_id
        status, environments = admin.call("GET", path + "/environments")
        assert status == 200
        env = next(item["id"] for item in environments if item["name"] == "development")
        safe = {"type": "boolean", "data": False}
        assert admin.call("POST", path + "/flags", {"environment_id": env, "key": "listing", "type": "boolean", "default": safe, "safe": safe, "reason": "Ingestion trial baseline"})[0] == 201
        status, run = admin.call("POST", path + "/experiments", {
            "environment_id": env, "flag_key": "listing", "expected_revision": 1,
            "name": "Ingestion trial", "control_variant_id": "control", "traffic_bp": 10000,
            "variants": [{"id": "control", "ordinal": 0, "weight_bp": 5000, "value": safe},
                         {"id": "treatment", "ordinal": 1, "weight_bp": 5000, "value": {"type": "boolean", "data": True}}],
            "reason": "Measure durable processing"})
        assert status == 201
        status, run = admin.call("POST", path + "/experiments/" + run["id"] + "/transitions", {"action": "start", "expected_revision": 1, "reason": "Start ingestion trial"})
        assert status == 200
        definition = run["definition"]
        status, key = admin.call("POST", path + "/application-keys", {"name": "Local ingestion trial", "environment_id": env, "permissions": ["evaluate", "events:write"]})
        assert status == 201

        def application(route, body):
            request = urllib.request.Request(base + route, data=json.dumps(body).encode(), headers={"Authorization": "Bearer " + key["token"], "Content-Type": "application/json"})
            try:
                response = urllib.request.urlopen(request, timeout=10)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                return response.status, json.load(response)

        for index in range(10):
            user = "trial_" + str(index)
            status, decision = application("/v1/evaluate", {"project_id": project_id, "environment_id": env, "key": "listing", "user_id": user, "fallback": safe})
            assert status == 200 and decision["reason"] == "experiment"
            assert decision["variant_id"] == bucket(definition, user)
            assert decision["revision"] == run["configuration_revision"]
        # The run freezes allocation at draft creation; starting it appends a flag revision.
        definition["revision"] = run["configuration_revision"]
        report["assignment_samples_verified"] = 10
        report["project_id"] = project_id
        report["run_id"] = run["id"]
        report["host"] = {"architecture": command(["uname", "-m"]),
                          "logical_cpus": command(["sysctl", "-n", "hw.logicalcpu"]),
                          "memory_bytes": command(["sysctl", "-n", "hw.memsize"]),
                          "docker_version": command(["docker", "version", "--format", "{{.Server.Version}}"])}

        def sql(query):
            return json.loads(command(COMPOSE + ["exec", "-T", "postgres", "psql", "-U", "switchyard", "-d", "switchyard", "-At", "-c", query]))

        def counts():
            return sql(f"""SELECT json_build_object(
              'raw',(SELECT count(*) FROM raw_events WHERE project_id='{project_id}'),
              'quarantined',(SELECT count(*) FROM raw_events WHERE project_id='{project_id}' AND status<>'accepted'),
              'intents',(SELECT count(*) FROM outbox WHERE project_id='{project_id}'),
              'published',(SELECT count(*) FROM outbox WHERE project_id='{project_id}' AND published_at IS NOT NULL),
              'receipts',(SELECT count(*) FROM processed_work WHERE reference->>'project_id'='{project_id}'),
              'due_users',(SELECT count(*) FROM metric_user_state WHERE project_id='{project_id}' AND due_at<=clock_timestamp()),
              'exposed',(SELECT COALESCE(sum(value),0) FROM metric_counts WHERE project_id='{project_id}' AND metric='exposed' AND category='provisional'),
              'converted',(SELECT COALESCE(sum(value),0) FROM metric_counts WHERE project_id='{project_id}' AND metric='converted' AND category='provisional'),
              'requests',(SELECT COALESCE(sum(value),0) FROM metric_counts WHERE project_id='{project_id}' AND category='request' AND metric='count'))""")

        deadline = time.monotonic() + 30
        while True:
            initial = counts()
            if initial["intents"] == initial["published"] == initial["receipts"]:
                break
            assert time.monotonic() < deadline, "Configuration did not drain"
            time.sleep(0.2)
        baseline = initial["intents"]
        now = datetime.datetime.now(datetime.timezone.utc)
        exposure_time = (now - datetime.timedelta(minutes=2)).isoformat()
        outcome_time = (now - datetime.timedelta(minutes=1)).isoformat()
        interval = 99 / args.rate
        batches = math.floor(args.seconds / interval)
        # Prebuild bounded payloads so JSON/hash generation does not distort the offered rate.
        payloads = []
        for batch in range(batches):
            items = []
            for offset in range(33):
                user = "trial_" + str(batch * 33 + offset)
                exposure = {"event_id": user + "_exposure", "kind": "exposure", "run_id": run["id"], "user_id": user,
                            "variant_id": bucket(definition, user), "revision": definition["revision"],
                            "decision_id": "synthetic_" + user, "decision_reason": "experiment", "occurred_at": exposure_time}
                completion = {**exposure, "event_id": user + "_completion", "kind": "listing_completion", "exposure_id": exposure["event_id"], "occurred_at": outcome_time}
                del completion["decision_id"]
                outcome = {**completion, "event_id": user + "_request", "kind": "request_outcome", "is_error": (batch * 33 + offset) % 100 == 0, "latency_ms": 200}
                items.extend([completion, outcome, exposure])
            payloads.append({"project_id": project_id, "environment_id": env, "events": items})

        started = time.monotonic()

        def observe():
            while not stop.is_set():
                try:
                    report["queue_samples"].append({"elapsed_seconds": round(time.monotonic()-started, 3), **counts()})
                    stats = command(["docker", "stats", "--no-stream", "--format", "{{json .}}"])
                    report["resource_samples"].append({"elapsed_seconds": round(time.monotonic()-started, 3), "containers": [json.loads(line) for line in stats.splitlines()]})
                except Exception as error:
                    report["observer_errors"].append(type(error).__name__)
                stop.wait(1)

        observer = threading.Thread(target=observe)
        observer.start()

        def submit(index, scheduled):
            begin = time.monotonic()
            try:
                status, data = application("/v1/events", payloads[index])
                receipts = data.get("receipts", [])
                valid = status == 200 and len(receipts) == 99 and all(item["status"] == "accepted" and not item["duplicate"] for item in receipts)
                return {"status": str(status), "valid_receipts": valid, "latency_seconds": time.monotonic()-begin,
                        "schedule_lateness_seconds": begin-scheduled, "completed_seconds": time.monotonic()-started}
            except Exception as error:
                return {"status": type(error).__name__, "valid_receipts": False, "latency_seconds": time.monotonic()-begin,
                        "schedule_lateness_seconds": begin-scheduled, "completed_seconds": time.monotonic()-started}

        inflight = set()
        dropped = 0
        with concurrent.futures.ThreadPoolExecutor(max_workers=16) as executor:
            for index in range(batches):
                scheduled = started + index * interval
                time.sleep(max(0, scheduled-time.monotonic()))
                finished = {future for future in inflight if future.done()}
                results.extend(future.result() for future in finished)
                inflight -= finished
                if len(inflight) >= 32:
                    dropped += 1
                else:
                    inflight.add(executor.submit(submit, index, scheduled))
                if index % 100 == 0:
                    print(f"Offered {index+1}/{batches} batches; inflight={len(inflight)} skipped={dropped}", flush=True)
            results.extend(future.result() for future in inflight)
        response_elapsed = time.monotonic()-started
        accepted = sum(item["valid_receipts"] for item in results) * 99
        status_counts = {}
        for item in results:
            status_counts[item["status"]] = status_counts.get(item["status"], 0)+1
        report.update({"scheduled_events": batches*99, "submitted_events": len(results)*99, "skipped_events": dropped*99,
                       "validated_http_accepted_events": accepted, "http_batch_statuses": status_counts,
                       "last_response_seconds": response_elapsed, "accepted_per_offering_second": accepted/args.seconds,
                       "accepted_per_response_elapsed_second": accepted/response_elapsed,
                       "http_latency_p95_seconds": percentile([item["latency_seconds"] for item in results], .95),
                       "schedule_lateness_p95_seconds": percentile([item["schedule_lateness_seconds"] for item in results], .95)})
        deadline = time.monotonic()+180
        while True:
            final = counts()
            if final["published"] == final["intents"] == final["receipts"] and final["due_users"] == 0 and final["requests"]*3+final["quarantined"] == final["raw"]:
                break
            print("Draining", json.dumps(final), flush=True)
            if time.monotonic() >= deadline:
                report["drain_timeout_seconds"] = 180
                report["counts_at_drain_timeout"] = final
                raise RuntimeError("Worker did not drain within 180 seconds")
            time.sleep(5)
        report["drain_after_responses_seconds"] = time.monotonic()-started-response_elapsed
        report["final_counts"] = final
        report["database_freshness"] = freshness(project_id)
        assert final["raw"] == accepted and final["quarantined"] == 0
        assert final["intents"] == baseline+accepted
        assert final["exposed"] == final["converted"] == final["requests"] == accepted//3
        status, retry = application("/v1/events", payloads[0])
        assert status == 200 and all(item["duplicate"] for item in retry["receipts"])
        assert counts() == final
        report["duplicate_retry_verified"] = True
        report["parity_output"] = command(["make", "aggregation-parity"])
        report["passed_correctness"] = True
    finally:
        stop.set()
        if observer is not None:
            observer.join(timeout=35)
        destination = pathlib.Path(".cache/ingestion-trial-report.json")
        destination.parent.mkdir(exist_ok=True)
        destination.write_text(json.dumps(report, indent=2)+"\n")
        if key is not None:
            assert admin.call("DELETE", path + "/application-keys/" + key["id"])[0] == 204
        assert admin.call("DELETE", "/v1/session")[0] == 204
    print(json.dumps({key: value for key, value in report.items() if key not in ("resource_samples", "queue_samples")}, indent=2), flush=True)


if __name__ == "__main__":
    main()
