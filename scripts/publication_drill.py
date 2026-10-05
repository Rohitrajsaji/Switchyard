"""Local M7 publisher checkpoint drill; no queues or database volumes are purged."""
import datetime
import json
from pathlib import Path
import subprocess
import time
import urllib.request
import uuid
from management_smoke import Client, base

ROOT = Path(__file__).resolve().parent.parent
COMPOSE = ["docker", "compose", "-f", str(ROOT / "compose.yaml"), "-p", "switchyard", "--profile", "async"]


def command(args, capture=False):
    return subprocess.run(args, cwd=ROOT, check=True, text=True, stdout=subprocess.PIPE if capture else None).stdout


def wait_for(label, predicate, seconds=20):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.1)
    raise RuntimeError("Timed out: " + label)


def main():
    if base not in {"http://localhost:8080", "http://127.0.0.1:8080"}:
        raise RuntimeError("Publication drill requires the default local stack")
    admin = Client("admin@example.test")
    nats_stopped = worker_stopped = False
    report = {}
    try:
        status, project = admin.call("POST", "/v1/projects", {"name": "Publication drill " + uuid.uuid4().hex[:8]})
        if status != 201:
            raise RuntimeError(f"Project setup HTTP {status}")
        project_id = project["id"]
        # Generated opaque IDs are validated before embedding into this fixed query.
        if not project_id.startswith("prj_") or not project_id[4:].isalnum():
            raise RuntimeError("Unexpected project identity")
        path = "/v1/projects/" + project_id
        status, envs = admin.call("GET", path + "/environments")
        if status != 200:
            raise RuntimeError("Environment read failed")
        env = next(e["id"] for e in envs if e["name"] == "development")
        cfg = {"default": {"type": "boolean", "data": True}, "safe": {"type": "boolean", "data": False}, "rules": [], "killed": False}
        status, definition = admin.call("POST", path + "/flags", {**cfg, "environment_id": env, "key": "publication", "type": "boolean", "reason": "Local publication drill"})
        if status != 201:
            raise RuntimeError(f"Flag setup HTTP {status}")

        def counts():
            sql = f"SELECT count(*),count(*) FILTER(WHERE published_at IS NOT NULL),count(*) FILTER(WHERE dead_at IS NOT NULL) FROM outbox WHERE project_id='{project_id}'"
            output = command(COMPOSE + ["exec", "-T", "postgres", "psql", "-U", "switchyard", "-d", "switchyard", "-qAt", "-c", sql], capture=True).strip()
            return tuple(map(int, output.split("|")))

        def messages():
            with urllib.request.urlopen("http://127.0.0.1:18229/jsz?streams=true", timeout=2) as response:
                details = json.load(response)
            for account in details.get("account_details", []):
                for stream in account.get("stream_detail", []):
                    if stream["name"] == "SWITCHYARD_WORK_V1":
                        return stream["state"]["messages"]
            raise RuntimeError("Application work stream missing")

        def update(killed):
            nonlocal definition
            status, definition = admin.call("PUT", path + "/flags/publication", {**cfg, "killed": killed, "environment_id": env,
                "expected_revision": definition["revision"], "reason": "Local durable publication drill"})
            if status != 200:
                raise RuntimeError(f"Configuration update HTTP {status}")

        wait_for("initial broker acknowledgement", lambda: counts() == (1, 1, 0))
        before = messages()
        command(COMPOSE + ["stop", "nats"])
        nats_stopped = True
        update(True)
        if counts() != (2, 1, 0):
            raise RuntimeError("Broker outage lost or falsely acknowledged publication intent")
        command(COMPOSE + ["up", "--no-build", "-d", "--wait", "nats"])
        nats_stopped = False
        if messages() < before:
            raise RuntimeError("Server restart lost retained messages")
        start = time.monotonic()
        wait_for("outbox recovery after broker restart", lambda: counts() == (2, 2, 0))
        report["publication_recovery_seconds"] = round(time.monotonic() - start, 4)
        command(COMPOSE + ["stop", "worker"])
        worker_stopped = True
        update(False)
        if counts() != (3, 2, 0):
            raise RuntimeError("Stopped worker falsely marked publication complete")
        command(COMPOSE + ["up", "--no-build", "-d", "--wait", "worker"])
        worker_stopped = False
        wait_for("worker restart resumes pending publication", lambda: counts() == (3, 3, 0))
        after = messages()
        if after < before + 2:
            raise RuntimeError("Retained work did not include recovered publications")
        report.update({"retained_messages_before": before, "retained_messages_after": after, "fixture_outbox_counts": list(counts())})
        output = ROOT / ".cache" / "publication-drill-report.json"
        output.write_text(json.dumps({"observed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(), **report}, indent=2) + "\n")
        print("Publisher drill passed: acknowledged file storage, broker/worker restart, durable pending intent and publication recovery")
        print(json.dumps(report, indent=2))
    finally:
        if nats_stopped:
            command(COMPOSE + ["up", "--no-build", "-d", "--wait", "nats"])
        if worker_stopped:
            command(COMPOSE + ["up", "--no-build", "-d", "--wait", "worker"])
        admin.call("DELETE", "/v1/session")


if __name__ == "__main__":
    main()
