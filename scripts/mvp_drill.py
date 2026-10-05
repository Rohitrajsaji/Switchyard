"""Fresh-volume MVP rehearsal. Removes only the unique rehearsal Compose project.

Uses already built images; run make up and install the browser dependencies first.
No credentials or API responses are printed. The normal development volume is untouched.
"""
import json
import hashlib
import os
from pathlib import Path
import socket
import subprocess
import sys
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parent.parent


def main():
    if not os.environ.get("SWITCHYARD_DEMO_PASSWORD"):
        raise RuntimeError("Set SWITCHYARD_DEMO_PASSWORD explicitly")
    # Discover available loopback ports; Compose still fails safely if another
    # process claims one between discovery and startup.
    sockets = []
    try:
        for _ in range(9):
            listener = socket.socket()
            listener.bind(("127.0.0.1", 0))
            sockets.append(listener)
        pg_port, api_port, web_port, redis_port, nats_port, monitor_port, grpc_port, api_metrics_port, worker_metrics_port = [s.getsockname()[1] for s in sockets]
    finally:
        for listener in sockets:
            listener.close()
    project = "switchyard-mvp-" + uuid.uuid4().hex[:12]
    api_url = f"http://localhost:{api_port}"
    web_url = f"http://localhost:{web_port}"
    env = {**os.environ, "POSTGRES_PORT": str(pg_port), "API_PORT": str(api_port),
           "WEB_PORT": str(web_port), "REDIS_PORT": str(redis_port), "NATS_PORT": str(nats_port),
           "NATS_MONITOR_PORT": str(monitor_port), "GRPC_PORT": str(grpc_port),
           "API_METRICS_PORT": str(api_metrics_port), "WORKER_METRICS_PORT": str(worker_metrics_port),
           "SWITCHYARD_URL": api_url,
           "SWITCHYARD_WEB_URL": web_url, "SWITCHYARD_ORIGIN": web_url,
           "SWITCHYARD_E2E_EXTERNAL": "true", "COMPOSE_PROJECT_NAME": project,
           "COMPOSE_FILE": str(ROOT / "compose.yaml"), "COMPOSE_PROFILES": "cache,async"}
    compose = ["docker", "compose", "-f", str(ROOT / "compose.yaml"), "-p", project]

    def run(command, capture=False):
        return subprocess.run(command, cwd=ROOT, env=env, check=True,
                              text=True, stdout=subprocess.PIPE if capture else None).stdout

    def counts():
        sql = """SELECT json_build_object(
          'users',(SELECT count(*) FROM users),
          'active_users',(SELECT count(*) FROM users WHERE active),
          'projects',(SELECT count(*) FROM projects),
          'memberships',(SELECT count(*) FROM project_memberships),
          'flags',(SELECT count(*) FROM flags),
          'runs',(SELECT count(*) FROM experiment_runs),
          'events',(SELECT count(*) FROM raw_events),
          'exposures',(SELECT count(*) FROM raw_events WHERE kind='exposure' AND status='accepted'),
          'completions',(SELECT count(*) FROM raw_events WHERE kind='listing_completion' AND status='accepted'),
          'requests',(SELECT count(*) FROM raw_events WHERE kind='request_outcome'),
          'active_keys',(SELECT count(*) FROM application_keys WHERE revoked_at IS NULL),
          'audits',(SELECT count(*) FROM audit_entries));"""
        return json.loads(run(compose + ["exec", "-T", "postgres", "psql", "-U", "switchyard",
                                        "-d", "switchyard", "-At", "-c", sql], capture=True))

    try:
        print("Starting an isolated MVP stack with a fresh PostgreSQL volume", flush=True)
        run(compose + ["up", "--no-build", "-d", "--wait"])
        run([sys.executable, "scripts/smoke.py"])
        run(["make", "seed-demo"])
        initial = counts()
        expected = {"users": 5, "active_users": 4, "projects": 1, "memberships": 4, "flags": 2, "runs": 1,
                    "events": 150, "exposures": 100, "completions": 50, "requests": 0, "active_keys": 0}
        if any(initial[key] != value for key, value in expected.items()):
            raise RuntimeError(f"Unexpected initial fixture counts: {initial}")
        run(["make", "seed-demo"])
        if counts() != initial:
            raise RuntimeError("Repeated seed changed facts, configuration or audits")
        print("Repeated opt-in seed preserved all fixture/audit counts", flush=True)
        run(["make", "e2e"])
        before_restart = counts()
        # Stop/start, rather than rebuilding, exercises persistence across normal restarts.
        run(compose + ["stop", "web", "api", "worker", "nats", "redis", "postgres"])
        run(compose + ["up", "--no-build", "-d", "--wait"])
        run([sys.executable, "scripts/smoke.py"])
        with urllib.request.urlopen(web_url, timeout=10) as response:
            if response.status != 200:
                raise RuntimeError("Dashboard did not recover after restart")
        run([sys.executable, "scripts/seed_demo.py"])
        if counts() != before_restart:
            raise RuntimeError("Restart/repeated bootstrap changed durable facts or audit history")
        print("MVP drill passed: fresh setup, seed idempotence, production browser journeys and restart persistence", flush=True)
    finally:
        # The UUID project was created by this drill, never the default project.
        subprocess.run(compose + ["down", "--volumes", "--remove-orphans"], cwd=ROOT, env=env, check=True)
        marker = ROOT / ".cache" / ("demo-seed-" + hashlib.sha256(api_url.encode()).hexdigest()[:12])
        for suffix in [".json", ".lock", ".tmp"]:
            marker.with_suffix(suffix).unlink(missing_ok=True)


if __name__ == "__main__":
    main()
