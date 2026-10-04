"""Verify real database outage and graceful API shutdown on the Switchyard stack."""
import json
import subprocess
import time
import urllib.error
import urllib.request


def compose(*args):
    return subprocess.check_output(["docker", "compose", *args], text=True)


def status(path):
    try:
        with urllib.request.urlopen("http://127.0.0.1:8080" + path, timeout=3) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code
    except urllib.error.URLError:
        return None


def wait_for(path, expected):
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        if status(path) == expected:
            return
        time.sleep(0.2)
    raise AssertionError(f"{path} did not reach {expected}")


wait_for("/health/ready", 200)
try:
    compose("stop", "postgres")
    wait_for("/health/ready", 503)
    assert status("/health/live") == 200, "database outage affected liveness"
finally:
    compose("up", "-d", "--wait", "postgres")
wait_for("/health/ready", 200)
compose("stop", "-t", "15", "api")
api_id = compose("ps", "-a", "-q", "api").strip()
state = json.loads(subprocess.check_output(["docker", "inspect", api_id], text=True))[0]["State"]
assert state["ExitCode"] == 0, state
logs = compose("logs", "--no-color", "api")
assert '"msg":"api shutdown complete"' in logs, "no graceful shutdown evidence"
compose("up", "-d", "--wait", "api")
wait_for("/health/ready", 200)
print("Database outage: ready=503/live=200; recovery=200; API SIGTERM exit=0; restart=200")
