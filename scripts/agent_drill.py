"""Live agent boundary: mock suggestion, Go rejection of bad output, human approval, no direct mutation."""
import json
import os
import subprocess
import sys
import urllib.error
import urllib.request
import uuid

from management_smoke import Client, base

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SAFE = {"type": "boolean", "data": False}
ON = {"type": "boolean", "data": True}


def bearer(method, path, token, data=None):
    request = urllib.request.Request(
        base + path,
        data=None if data is None else json.dumps(data).encode(),
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"},
        method=method,
    )
    try:
        response = urllib.request.urlopen(request, timeout=10)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        body = response.read()
        return response.code, json.loads(body) if body else None


def main():
    admin = Client("admin@example.test")
    reviewer = Client("reviewer@example.test")
    developer = Client("developer@example.test")
    status, project = admin.call("POST", "/v1/projects", {"name": "Agent " + uuid.uuid4().hex[:8]})
    assert status == 201, project
    path = "/v1/projects/" + project["id"]
    for user in ("demo_reviewer", "demo_developer"):
        assert admin.call("POST", path + "/members", {"user_id": user})[0] == 204
    environments = admin.call("GET", path + "/environments")[1]
    prod = next(e["id"] for e in environments if e["name"] == "production")
    status, key = developer.call("POST", path + "/application-keys", {
        "name": "Listing agent", "environment_id": prod,
        "permissions": ["context:read", "proposals:submit"]})
    assert status == 201 and key["token"].startswith("swk_"), key
    assert developer.call("POST", path + "/application-keys", {
        "name": "too broad", "environment_id": prod,
        "permissions": ["evaluate", "proposals:submit"]})[0] == 400

    context_path = path + "/agent/context?environment_id=" + prod
    status, summary = bearer("GET", context_path, key["token"])
    assert status == 200 and summary["flags"] == [] and summary["environment"] == "production", summary
    cli = [sys.executable, "-m", "switchyard_agent.cli", "--project", project["id"], "--environment", prod, "--dry-run"]
    env = {**os.environ, "PYTHONPATH": os.path.join(ROOT, "agent")}
    # The drill uses the installed test environment when present so pydantic imports resolve.
    python = os.path.join(ROOT, ".cache", "agent-venv", "bin", "python")
    if os.path.exists(python):
        cli[0] = python
    suggested = json.loads(subprocess.check_output(cli, env=env, cwd=ROOT, text=True))
    assert suggested["kind"] == "create" and suggested["rollout"]["traffic_bp"] == 1000, suggested

    jump = dict(suggested)
    jump["rollout"] = {"traffic_bp": 5000, "value": ON}
    jump["rationale"] = "jump past the cap"
    assert bearer("POST", path + "/agent/proposals", key["token"], jump)[0] == 400
    sensitive = dict(suggested)
    sensitive["rules"] = [{"attribute": "email", "operator": "eq", "values": ["a@b.test"], "value": ON}]
    sensitive["rationale"] = "target a person"
    assert bearer("POST", path + "/agent/proposals", key["token"], sensitive)[0] == 400
    assert bearer("PUT", path + "/flags/listing", key["token"], {
        "environment_id": prod, "expected_revision": 1, "reason": "direct",
        "default": SAFE, "safe": SAFE, "killed": False, "rules": [], "rollout": {"traffic_bp": 1000, "value": ON}})[0] == 401

    status, proposal = bearer("POST", path + "/agent/proposals", key["token"], suggested)
    assert status == 201 and proposal["source"] == "agent" and proposal["proposer_id"] == "demo_developer", proposal
    assert bearer("POST", f"{path}/proposals/{proposal['id']}/apply", key["token"])[0] == 401
    assert developer.call("POST", f"{path}/proposals/{proposal['id']}/approve", {
        "diff_hash": proposal["diff_hash"], "reason": "self"})[0] == 403
    assert reviewer.call("POST", f"{path}/proposals/{proposal['id']}/approve", {
        "diff_hash": proposal["diff_hash"], "reason": "reviewed the agent diff"})[0] == 200
    status, applied = developer.call("POST", f"{path}/proposals/{proposal['id']}/apply")
    assert status == 200 and applied["applied_revision"] == 1, applied
    status, again = admin.call("POST", f"{path}/proposals/{proposal['id']}/apply")
    assert status == 200 and again["applied_revision"] == 1
    flag = admin.call("GET", f"{path}/flags/listing?environment_id={prod}")[1]
    assert flag["rollout"]["traffic_bp"] == 1000 and flag["revision"] == 1
    print("agent drill passed: mock suggestion, cap, sensitive targeting, no direct mutation, second-person approval, one apply")
    assert admin.call("DELETE", "/v1/session")[0] == 204
    assert reviewer.call("DELETE", "/v1/session")[0] == 204
    assert developer.call("DELETE", "/v1/session")[0] == 204


if __name__ == "__main__":
    main()
