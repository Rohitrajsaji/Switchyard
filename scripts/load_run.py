"""Repeatable local load runs. Runs the pinned k6 image on the compose network, samples container
CPU/memory, verifies correctness and writes a self-describing JSON report.

    python3 scripts/load_run.py evaluation --steps 500,1000,2000 --step-seconds 60
    python3 scripts/load_run.py events --batch-rate 10 --seconds 120
    python3 scripts/load_run.py mixed --steps 1000 --step-seconds 120 --batch-rate 10
    python3 scripts/load_run.py soak --steps 1000 --step-seconds 600 --batch-rate 5

Failing a target is a finding: the report records what was achieved and which gates failed."""
import argparse
import datetime
import json
import pathlib
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

from management_smoke import Client
from ingestion_trial import freshness

ROOT = pathlib.Path(__file__).resolve().parent.parent
K6_IMAGE = "grafana/k6:2.3.0@sha256:9c2dee7f8ed74d317e4027c06a10f169b625638189de8d4555d0b3486a5aeb34"
SERVICES = ["switchyard-api-1", "switchyard-worker-1", "switchyard-postgres-1", "switchyard-redis-1", "switchyard-nats-1"]
PROMETHEUS = "http://127.0.0.1:9091"


def run(args, **kwargs):
    return subprocess.run(args, capture_output=True, text=True, check=kwargs.pop("check", True), **kwargs).stdout.strip()


def parse_memory(text):
    used = text.split("/")[0].strip()
    for suffix, factor in (("GiB", 1024), ("MiB", 1), ("KiB", 1 / 1024), ("B", 1 / 1048576)):
        if used.endswith(suffix):
            return float(used[: -len(suffix)]) * factor
    return 0.0


class Sampler(threading.Thread):
    """Samples docker stats for the stack and the k6 generator until stopped."""

    def __init__(self, extra):
        super().__init__(daemon=True)
        self.stop_event = threading.Event()
        self.names = SERVICES + [extra]
        self.samples = []

    def run(self):
        while not self.stop_event.is_set():
            out = run(["docker", "stats", "--no-stream", "--format", "{{json .}}"] + self.names, check=False)
            now = time.time()
            for line in out.splitlines():
                try:
                    row = json.loads(line)
                    self.samples.append({"t": now, "name": row["Name"], "cpu_percent": float(row["CPUPerc"].rstrip("%")), "memory_mib": parse_memory(row["MemUsage"])})
                except (ValueError, KeyError):
                    pass
            self.stop_event.wait(3)

    def summary(self):
        result = {}
        for name in sorted({s["name"] for s in self.samples}):
            rows = [s for s in self.samples if s["name"] == name]
            cpu = sorted(r["cpu_percent"] for r in rows)
            result[name] = {"samples": len(rows), "cpu_percent_mean": round(sum(cpu) / len(cpu), 1), "cpu_percent_max": cpu[-1],
                            "memory_mib_max": round(max(r["memory_mib"] for r in rows), 1)}
        return result


def prometheus_peaks(start, end):
    """Peak values of key gauges over the run window, or None when Prometheus is not running."""
    queries = {"consumer_pending_max": "max(switchyard_consumer_pending_messages)", "outbox_oldest_age_seconds_max": "max(switchyard_outbox_oldest_unpublished_age_seconds)",
               "cache_oldest_verification_age_seconds_max": "max(switchyard_cache_oldest_verification_age_seconds)", "db_pool_in_use_max": "max(switchyard_db_pool_acquired_connections)",
               "goroutines_max": "max(go_goroutines)"}
    out = {}
    try:
        for name, query in queries.items():
            url = PROMETHEUS + "/api/v1/query_range?" + urllib.parse.urlencode({"query": query, "start": start, "end": end, "step": 5})
            with urllib.request.urlopen(url, timeout=5) as response:
                series = json.load(response)["data"]["result"]
            out[name] = max((float(v[1]) for s in series for v in s["values"]), default=None)
    except OSError:
        return None
    return out


def stages(steps, hold, warmup):
    result = [{"target": min(steps[0], 200), "duration": f"{warmup}s"}]
    for rate in steps:
        result += [{"target": rate, "duration": "10s"}, {"target": rate, "duration": f"{hold}s"}]
    return result


def metric(summary, name, key):
    return summary.get("metrics", {}).get(name, {}).get(key)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("workload", choices=["evaluation", "events", "mixed", "soak"])
    parser.add_argument("--steps", default="500", help="comma-separated evaluation arrival rates (requests/s)")
    parser.add_argument("--step-seconds", type=int, default=60)
    parser.add_argument("--warmup-seconds", type=int, default=30)
    parser.add_argument("--batch-rate", type=int, default=10, help="event batches/s (99 events each)")
    parser.add_argument("--seconds", type=int, default=0, help="events duration; defaults to the evaluation duration")
    parser.add_argument("--name", default="")
    parser.add_argument("--max-vus", type=int, default=200, help="k6 VUs cost ~8 MiB each; the Docker VM here has 4 GiB")
    parser.add_argument("--reuse-fixture", action="store_true", help="evaluation-only runs may reuse the last fixture")
    parser.add_argument("--generator", choices=["go", "k6"], default="go", help="evaluation-only generator; k6 cannot reach high rates in the 4 GiB Docker VM")
    parser.add_argument("--transport", choices=["http", "grpc"], default="http", help="evaluation transport for the go generator")
    args = parser.parse_args()
    steps = [int(x) for x in args.steps.split(",")]
    evaluation = args.workload in ("evaluation", "mixed", "soak")
    ingest = args.workload in ("events", "mixed", "soak")
    k6_workload = "evaluation" if args.workload == "evaluation" else "events" if args.workload == "events" else "mixed"
    event_seconds_default = args.warmup_seconds + sum(10 + args.step_seconds for _ in steps)
    event_seconds = args.seconds or (event_seconds_default if evaluation else 60)
    name = args.name or f"{args.workload}-{datetime.datetime.now(datetime.timezone.utc):%Y%m%dT%H%M%SZ}"
    out_dir = ROOT / "loadtest" / "out"
    out_dir.mkdir(exist_ok=True)
    fixture_path = ROOT / "loadtest" / "fixture.json"
    # Event runs need a fresh run so the exact-count gate is not affected by earlier users.
    if ingest or not fixture_path.exists() or not args.reuse_fixture:
        print("creating a fresh fixture ...", flush=True)
        print(run([sys.executable, str(ROOT / "scripts" / "load_fixture.py")], cwd=str(ROOT / "scripts")), flush=True)
    fixture = json.loads(fixture_path.read_text())
    nonce = uuid.uuid4().hex[:8]
    # evaluation-only runs execute one short k6 invocation per rate step: k6 keeps every sample in
    # memory and the Docker VM here has 4 GiB, so long single runs are killed by the OOM killer.
    plans = []
    if args.workload == "evaluation":
        for index, rate in enumerate(steps):
            warm = [{"target": min(rate, 200), "duration": f"{args.warmup_seconds}s"}] if index == 0 else []
            plans.append({"label": f"step-{rate}", "rate": rate, "workload": "evaluation", "seconds": sum(int(x["duration"][:-1]) for x in warm) + 10 + args.step_seconds,
                          "stages": warm + [{"target": rate, "duration": "10s"}, {"target": rate, "duration": f"{args.step_seconds}s"}]})
    else:
        plans.append({"label": "run", "rate": steps[0], "workload": "events" if args.workload == "events" else "mixed", "seconds": event_seconds if not evaluation else event_seconds_default,
                      "stages": stages(steps, args.step_seconds, args.warmup_seconds)})
    report = {"name": name, "workload": args.workload, "commit": run(["git", "rev-parse", "--short", "HEAD"], cwd=str(ROOT)),
              "dirty_tree": bool(run(["git", "status", "--porcelain", "--untracked-files=no"], cwd=str(ROOT))),
              "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "host": {"cpu": run(["sysctl", "-n", "machdep.cpu.brand_string"], check=False), "cpus": int(run(["sysctl", "-n", "hw.ncpu"], check=False) or 0),
                       "memory_gib": round(int(run(["sysctl", "-n", "hw.memsize"], check=False) or 0) / 2**30, 1),
                       "docker_vm_memory_gib": round(int(run(["docker", "info", "--format", "{{.MemTotal}}"], check=False) or 0) / 2**30, 1)},
              "docker_memory_limits_mib": {n: round(int(run(["docker", "inspect", "--format", "{{.HostConfig.Memory}}", n], check=False) or 0) / 2**20) for n in SERVICES},
              "observability_profile_running": bool(run(["docker", "ps", "-q", "--filter", "name=switchyard-prometheus-1"], check=False)),
              "k6_image": K6_IMAGE, "max_vus": args.max_vus,
              "dataset": {"flags": len(fixture["flags"]), "experiments": sum(1 for f in fixture["flags"] if f["kind"] == "experiment"), "evaluation_user_space": 100000, "event_user_pool": len(fixture["users"])}}
    sampler = Sampler("switchyard-k6")
    started = time.time()
    report["generator_kind"] = "go" if args.workload == "evaluation" and args.generator == "go" else "k6"
    summaries, exit_codes, step_rows = {}, [], []
    go_generator = args.workload == "evaluation" and args.generator == "go"
    if go_generator:
        plans = []
        print(run(["docker", "build", "-q", "-t", "switchyard-loadgen:local", "-f", "deploy/docker/loadgen.Dockerfile", "."], cwd=str(ROOT)), flush=True)
        container = "switchyard-loadgen-" + uuid.uuid4().hex[:6]
        sampler.names = SERVICES + [container]
        sampler.start()
        label_file = f"{name}-{args.transport}"
        total = args.warmup_seconds + len(steps) * (10 + args.step_seconds + 3)
        command = ["docker", "run", "--rm", "--name", container, "--network", "switchyard_default", "-v", f"{ROOT / 'loadtest'}:/loadtest", "switchyard-loadgen:local",
                   "-transport", args.transport, "-rates", args.steps, "-hold", f"{args.step_seconds}s", "-ramp", "10s", "-warmup", f"{args.warmup_seconds}s",
                   "-max-in-flight", "4000", "-out", f"/loadtest/out/{label_file}.json"]
        print(f"running loadgen {args.transport} steps {args.steps} (~{total}s) ...", flush=True)
        proc = subprocess.run(command, capture_output=True, text=True)
        exit_codes.append(proc.returncode)
        out_file = out_dir / f"{label_file}.json"
        rows = json.loads(out_file.read_text()) if out_file.exists() else []
        if not rows:
            report["loadgen_stderr_tail"] = proc.stderr[-1500:]
        for row in rows:
            step_rows.append({"target_requests_per_second": row["target_requests_per_second"], "transport": row["transport"], "requests": row["requests"],
                "achieved_requests_per_second_during_hold": round(row["achieved_requests_per_second_during_hold"], 1), "latency_ms": row["latency_ms"],
                "failure_rate": row["failure_rate"], "dropped_iterations": row["dropped_iterations"], "correctness_rate": row["correctness_rate"], "max_in_flight": row["max_in_flight"]})
        report["generator"] = {"name": "cmd/loadgen", "stderr_progress": proc.stderr.strip().splitlines()[-len(steps):]}
    for plan in plans:
        container = "switchyard-k6-" + uuid.uuid4().hex[:6]
        sampler.names = SERVICES + [container]
        if not sampler.is_alive():
            sampler.start()
        env = {"WORKLOAD": plan["workload"], "FIXTURE": "/loadtest/fixture.json", "BASE_URL": "http://api:8080", "NONCE": nonce,
               "EVAL_STAGES": json.dumps(plan["stages"]), "EVAL_START": str(min(plan["rate"], 200)), "EVAL_VUS": "40", "EVAL_MAX_VUS": str(args.max_vus),
               "EVENT_BATCH_RATE": str(args.batch_rate), "EVENT_DURATION": f"{event_seconds}s"}
        label_file = f"{name}-{plan['label']}"
        command = ["docker", "run", "--rm", "--name", container, "--network", "switchyard_default", "-v", f"{ROOT / 'loadtest'}:/loadtest"]
        for key, value in env.items():
            command += ["-e", f"{key}={value}"]
        command += [K6_IMAGE, "run", "--summary-export", f"/loadtest/out/{label_file}.json", "--quiet", "/loadtest/workload.js"]
        print(f"running k6 {plan['label']} ({plan['seconds']}s) ...", flush=True)
        step_started = time.time()
        proc = subprocess.run(command, capture_output=True, text=True)
        exit_codes.append(proc.returncode)
        summary_file = out_dir / f"{label_file}.json"
        summary = json.loads(summary_file.read_text()) if summary_file.exists() else {}
        summaries[plan["label"]] = summary
        if not summary:
            report.setdefault("k6_stderr_tail", proc.stderr[-1500:])
        if evaluation:
            d = "http_req_duration{kind:evaluate}"
            hold = args.step_seconds
            count = metric(summary, "http_reqs", "count")
            step_rows.append({"target_requests_per_second": plan["rate"], "requests": count, "achieved_requests_per_second_during_hold_estimate": round(count / plan["seconds"], 1) if count else None,
                "latency_ms": {k: metric(summary, d, k) for k in ("avg", "med", "p(90)", "p(95)", "p(99)", "max")},
                "failure_rate": metric(summary, "http_req_failed{kind:evaluate}", "value"), "dropped_iterations": metric(summary, "dropped_iterations", "count") or 0,
                "correctness_rate": metric(summary, "evaluation_correct", "value"), "max_vus": metric(summary, "vus_max", "max"), "k6_exit_code": proc.returncode,
                "seconds": plan["seconds"], "wall_seconds": round(time.time() - step_started, 1)})
    finished = time.time()
    sampler.stop_event.set()
    sampler.join(timeout=10)
    report["generator_exit_codes"] = exit_codes
    report["duration_seconds"] = round(finished - started, 1)
    summary = summaries.get("run", {})
    if evaluation:
        report["evaluation_steps"] = step_rows
        if args.workload != "evaluation":
            only = step_rows[0] if step_rows else {}
            report["evaluation_mixed"] = only
    if ingest:
        batches = int(metric(summary, "event_batches_accepted", "count") or 0)
        rejected = int(metric(summary, "event_batches_rejected", "count") or 0)
        accepted_events = int(metric(summary, "events_accepted", "count") or 0)
        d = "http_req_duration{kind:ingest}"
        statuses = {name.split("{status:")[-1].rstrip("}"): int(body.get("count") or 0)
                    for name, body in summary.get("metrics", {}).items() if name.startswith("ingest_http_status{")}
        ingestion = report["ingestion"] = {"batch_rate_requested": args.batch_rate, "events_per_second_requested": args.batch_rate * 99, "accepted_batches": batches, "rejected_batches": rejected,
            "accepted_events": accepted_events, "accepted_events_per_second": round(accepted_events / max(1, event_seconds), 1),
            "batch_latency_ms": {k: metric(summary, d, k) for k in ("avg", "med", "p(90)", "p(95)", "p(99)", "max")},
            "failure_rate": metric(summary, "http_req_failed{kind:ingest}", "value"), "dropped_iterations": metric(summary, "dropped_iterations", "count"),
            "http_status_counts": statuses}
        # Convergence and exact counts through the public results API, after the load ends.
        # A slow or refused read is a result, not a reason to discard the generator summary.
        admin = Client("admin@example.test")
        path = f"/v1/projects/{fixture['project_id']}/experiments/{fixture['run_id']}/results"
        distinct = min(batches * 33, len(fixture["users"]))
        deadline = time.monotonic() + 600
        converged_at, counts = None, [0, 0, 0]
        read_errors = 0
        while time.monotonic() < deadline:
            try:
                status, results = admin.call("GET", path, timeout=20)
            except (TimeoutError, urllib.error.URLError):
                read_errors += 1
                time.sleep(2)
                continue
            if status == 200 and results:
                p = results["processing"]
                counts = [sum(v["total"]["exposed"] for v in results["variants"]), sum(v["total"]["converted"] for v in results["variants"]), sum(v["requests"]["count"] for v in results["variants"])]
                if p["pending_events"] == 0 and p["due_users"] == 0 and counts == [distinct] * 3:
                    converged_at = time.time()
                    break
            else:
                read_errors += 1
            time.sleep(2)
        ingestion["expected_distinct_users"] = distinct
        ingestion["converged_exactly"] = converged_at is not None
        ingestion["results_read_errors"] = read_errors
        ingestion["seconds_from_load_end_to_exact_convergence"] = round(converged_at - finished, 1) if converged_at else None
        ingestion["final_counts"] = {"exposed": counts[0], "converted": counts[1], "requests": counts[2]}
        try:
            ingestion["database_freshness_per_user_seconds"] = freshness(fixture["project_id"])
        except Exception as error:  # diagnostics only; the exact-count gate above is authoritative
            ingestion["database_freshness_error"] = type(error).__name__
        admin.call("DELETE", "/v1/session")
    report["resources"] = sampler.summary()
    report["prometheus_peaks"] = prometheus_peaks(started, finished)
    report["finished_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    destination = ROOT / "docs" / "benchmarks" / f"m10-{name}.json"
    destination.write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({k: v for k, v in report.items() if k not in ("k6_environment", "docker_memory_limits_mib")}, indent=2))
    print("report:", destination.relative_to(ROOT))


if __name__ == "__main__":
    main()
