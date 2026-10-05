"""Real local M6 propagation/repair/expiry drill; preserves project/audit history.

Creates one temporary API observer container. Stops/restarts local Redis and
briefly locks configuration reads in PostgreSQL; authorization remains available.
"""
import concurrent.futures
import datetime
import hashlib
import http.client
import json
from pathlib import Path
import selectors
import socket
import subprocess
import time
import urllib.error
import urllib.request
import uuid

from management_smoke import Client, base

ROOT = Path(__file__).resolve().parent.parent
COMPOSE = ["docker", "compose", "-f", str(ROOT / "compose.yaml"), "-p", "switchyard", "--profile", "cache"]


def command(args, capture=False):
    return subprocess.run(args, cwd=ROOT, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def wait_for(description, predicate, seconds=10):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if predicate():
            return
        time.sleep(0.1)
    raise RuntimeError("Timed out: " + description)


def main():
    # This drill controls the default local Docker stack only.
    if base not in {"http://localhost:8080", "http://127.0.0.1:8080"}:
        raise RuntimeError("Cache drill requires the default local API")
    admin = Client("admin@example.test")
    observer = None
    credential = None
    lock_process = None
    redis_stopped = False
    report = {}
    try:
        status, project = admin.call("POST", "/v1/projects", {"name": "Cache drill " + uuid.uuid4().hex[:8]})
        if status != 201:
            raise RuntimeError(f"Project setup HTTP {status}")
        path = "/v1/projects/" + project["id"]
        status, environments = admin.call("GET", path + "/environments")
        if status != 200:
            raise RuntimeError(f"Environment setup HTTP {status}")
        env = next(e["id"] for e in environments if e["name"] == "development")
        configuration = {"default": {"type": "boolean", "data": True}, "safe": {"type": "boolean", "data": False}, "rules": [], "killed": False}
        status, definition = admin.call("POST", path + "/flags", {**configuration, "environment_id": env,
                                       "key": "cache_listing", "type": "boolean", "reason": "Local M6 failure drill"})
        if status != 201:
            raise RuntimeError(f"Flag setup HTTP {status}")
        status, credential = admin.call("POST", path + "/application-keys", {"environment_id": env,
                                          "name": "Cache drill", "permissions": ["evaluate"]})
        if status != 201:
            raise RuntimeError(f"Credential setup HTTP {status}")
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        observer = command(COMPOSE + ["run", "-d", "--rm", "--no-deps", "--name", "switchyard-cache-drill-" + uuid.uuid4().hex[:12],
                             "-p", f"127.0.0.1:{port}:8080", "api"], capture=True).strip()
        observer_url = f"http://127.0.0.1:{port}"

        def healthy():
            try:
                with urllib.request.urlopen(observer_url + "/health/ready", timeout=2) as response:
                    return response.status == 200
            except (urllib.error.URLError, TimeoutError, http.client.RemoteDisconnected):
                return False

        wait_for("observer API startup", healthy, 30)

        def evaluate():
            request = urllib.request.Request(observer_url + "/v1/evaluate", method="POST",
                data=json.dumps({"project_id": project["id"], "environment_id": env, "key": "cache_listing",
                                 "user_id": "synthetic_cache_user", "fallback": configuration["safe"]}).encode(),
                headers={"Content-Type": "application/json", "Authorization": "Bearer " + credential["token"]})
            try:
                response = urllib.request.urlopen(request, timeout=5)
            except urllib.error.HTTPError as e:
                response = e
            with response:
                return response.status, json.load(response)

        scope = json.dumps({"ProjectID": project["id"], "EnvironmentID": env, "FlagKey": "cache_listing"}, separators=(",", ":"))
        redis_key = "switchyard:snapshot:v1:" + hashlib.sha256(scope.encode()).hexdigest()

        def payload():
            body = command(COMPOSE + ["exec", "-T", "redis", "redis-cli", "--raw", "HGET", redis_key, "payload"], capture=True).strip()
            return json.loads(body) if body else None

        status, result = evaluate()
        if status != 200 or result["value"]["data"] is not True:
            raise RuntimeError("Initial observer evaluation failed")
        with concurrent.futures.ThreadPoolExecutor(max_workers=16) as pool:
            for status, result in pool.map(lambda _: evaluate(), range(32)):
                if status != 200 or result["revision"] != 1 or result["value"]["data"] is not True:
                    raise RuntimeError("Concurrent warm evaluations changed assignment")
        wait_for("initial Redis snapshot", lambda: payload() is not None)

        def update(killed):
            nonlocal definition
            status, updated = admin.call("PUT", path + "/flags/cache_listing", {**configuration, "killed": killed,
                "environment_id": env, "expected_revision": definition["revision"], "reason": "Local cache propagation drill"})
            if status != 200:
                raise RuntimeError(f"Flag transition HTTP {status}")
            definition = updated

        update(True)
        start = time.monotonic()

        def sees(killed):
            status, value = evaluate()
            return status == 200 and value["revision"] == definition["revision"] and value["value"]["data"] is (not killed)

        wait_for("disable propagates to independent API", lambda: sees(True), 5)
        report["healthy_disable_seconds"] = round(time.monotonic() - start, 4)
        command(COMPOSE + ["exec", "-T", "redis", "redis-cli", "DEL", redis_key], capture=True)
        wait_for("deleted cache repaired", lambda: (payload() or {}).get("definition", {}).get("revision") == definition["revision"], 5)

        command(COMPOSE + ["stop", "redis"])
        redis_stopped = True
        update(False)
        start = time.monotonic()
        wait_for("PostgreSQL refresh during Redis outage", lambda: sees(False), 5)
        report["redis_outage_refresh_seconds"] = round(time.monotonic() - start, 4)
        if not healthy():
            raise RuntimeError("Redis outage broke API readiness")
        command(COMPOSE + ["up", "--no-build", "-d", "--wait", "redis"])
        redis_stopped = False
        wait_for("Redis restart repaired", lambda: (payload() or {}).get("definition", {}).get("revision") == definition["revision"], 5)

        # Keep a transaction open after taking a configuration-only lock. Source
        # queries time out; application-key checks still read their normal tables.
        lock_process = subprocess.Popen(COMPOSE + ["exec", "-T", "postgres", "psql", "-U", "switchyard", "-d", "switchyard", "-qAt"],
                                        cwd=ROOT, text=True, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        lock_process.stdin.write("BEGIN; LOCK TABLE flag_revisions IN ACCESS EXCLUSIVE MODE; SELECT 'LOCKED';\n")
        lock_process.stdin.flush()
        selector = selectors.DefaultSelector()
        selector.register(lock_process.stdout, selectors.EVENT_READ)
        try:
            if not selector.select(10) or lock_process.stdout.readline().strip() != "LOCKED":
                raise RuntimeError("Could not establish controlled configuration outage")
        finally:
            selector.close()
        print("Configuration reads locked; observing the real 30-second freshness boundary", flush=True)
        start = time.monotonic()
        last_good = None
        while time.monotonic() - start < 40:
            status, value = evaluate()
            if status == 503:
                if value["reason"] != "cache_expired" or value["value"]["data"] is not False or value.get("run_id"):
                    raise RuntimeError("Expired snapshot did not fail safely")
                report["fallback_seconds_after_lock"] = round(time.monotonic() - start, 4)
                break
            if status != 200 or value["value"]["data"] is not True:
                raise RuntimeError(f"Unexpected controlled-outage evaluation HTTP {status}")
            if time.monotonic() - start > 30.25:
                raise RuntimeError("An enabled decision was served beyond the bounded freshness window")
            last_good = time.monotonic()
            time.sleep(0.25)
        else:
            raise RuntimeError("Snapshot outlived its 30-second authoritative age")
        if last_good is None:
            raise RuntimeError("No last-known-good window was observed")
        lock_process.stdin.write("COMMIT;\n")
        lock_process.stdin.close()
        lock_process.wait(timeout=10)
        if lock_process.returncode != 0:
            raise RuntimeError("Configuration lock transaction failed")
        lock_process = None
        wait_for("PostgreSQL source recovery", lambda: sees(False), 5)
        status, _ = admin.call("DELETE", path + "/application-keys/" + credential["id"])
        if status != 204:
            raise RuntimeError(f"Revocation HTTP {status}")
        # The API that committed the revocation drops its own cache immediately.
        # The observer did not process it, so its positive lookup can survive for
        # the two-second application-key TTL and must then fail closed.
        local_request = urllib.request.Request(base + "/v1/evaluate", method="POST",
            data=json.dumps({"project_id": project["id"], "environment_id": env, "key": "cache_listing",
                             "user_id": "synthetic_cache_user", "fallback": configuration["safe"]}).encode(),
            headers={"Content-Type": "application/json", "Authorization": "Bearer " + credential["token"]})
        try:
            local_response = urllib.request.urlopen(local_request, timeout=5)
        except urllib.error.HTTPError as error:
            local_response = error
        with local_response:
            if local_response.status != 401:
                raise RuntimeError("Processing API kept a revoked application key")
        revoked_at = time.monotonic()
        wait_for("observer honors revocation after the auth cache TTL", lambda: evaluate()[0] == 401, 5)
        report["observer_revocation_seconds"] = round(time.monotonic() - revoked_at, 4)
        credential = None
        logs = command(["docker", "logs", observer], capture=True)
        counters = [json.loads(line)["statistics"] for line in logs.splitlines()
                    if line.startswith("{") and json.loads(line).get("msg") == "cache statistics"]
        if not counters or counters[-1]["MemoryHits"] < 32 or counters[-1]["SourceFailures"] == 0 or counters[-1]["StoreFailures"] == 0:
            raise RuntimeError("Live cache counters did not reflect warm hits and controlled failures")
        report["observer_counters"] = counters[-1]
        output = ROOT / ".cache" / "cache-drill-report.json"
        output.write_text(json.dumps({"observed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), **report}, indent=2) + "\n")
        print("Cache drill passed: independent propagation, deletion/restart repair, Redis outage, strict expiry, recovery and revocation", flush=True)
        print(json.dumps(report, indent=2), flush=True)
    finally:
        if lock_process is not None:
            # Closing psql's input rolls back the transaction even on failure.
            lock_process.stdin.close()
            try:
                lock_process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                lock_process.terminate()
                lock_process.wait(timeout=10)
        if redis_stopped:
            command(COMPOSE + ["up", "--no-build", "-d", "--wait", "redis"])
        if credential is not None:
            admin.call("DELETE", path + "/application-keys/" + credential["id"])
        if observer:
            command(["docker", "stop", observer], capture=True)
        admin.call("DELETE", "/v1/session")


if __name__ == "__main__":
    main()
