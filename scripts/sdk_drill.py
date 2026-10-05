"""Live Go SDK journey: gRPC evaluation, SDK event delivery, HTTP parity and revoked-key fallback.

Requires the Docker stack (gRPC on 127.0.0.1:9090) and opt-in seeded demo credentials."""
import json
import os
import re
import subprocess
import time
import urllib.request
import uuid

from management_smoke import Client, base


def run_example(env_vars):
    done = subprocess.run(["go", "run", "./examples/listing"], env={**os.environ, **env_vars},
                          capture_output=True, text=True, timeout=120,
                          cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    return done.returncode, done.stdout, done.stderr


def main():
    admin = Client("admin@example.test")
    status, project = admin.call("POST", "/v1/projects", {"name": "SDK " + uuid.uuid4().hex[:8]})
    assert status == 201
    path = "/v1/projects/" + project["id"]
    status, environments = admin.call("GET", path + "/environments")
    env = {item["name"]: item["id"] for item in environments}
    safe, treatment = {"type": "boolean", "data": False}, {"type": "boolean", "data": True}
    assert admin.call("POST", path + "/flags", {"environment_id": env["development"], "key": "listing_flow",
        "type": "boolean", "default": safe, "safe": safe, "reason": "sdk drill"})[0] == 201
    status, run = admin.call("POST", path + "/experiments", {
        "environment_id": env["development"], "flag_key": "listing_flow", "expected_revision": 1,
        "name": "SDK listing", "control_variant_id": "control", "traffic_bp": 10000,
        "variants": [{"id": "control", "ordinal": 0, "weight_bp": 5000, "value": safe},
                     {"id": "treatment", "ordinal": 1, "weight_bp": 5000, "value": treatment}],
        "reason": "sdk drill"})
    assert status == 201
    assert admin.call("POST", path + "/experiments/" + run["id"] + "/transitions",
                      {"action": "start", "expected_revision": 1, "reason": "sdk drill"})[0] == 200
    status, key = admin.call("POST", path + "/application-keys", {
        "name": "SDK drill", "environment_id": env["development"], "permissions": ["evaluate", "events:write"]})
    assert status == 201
    users = ["sdk-user-%d" % i for i in range(6)]
    common = {"SWITCHYARD_PROJECT_ID": project["id"], "SWITCHYARD_ENVIRONMENT_ID": env["development"],
              "SWITCHYARD_API_KEY": key["token"], "SWITCHYARD_GRPC_ADDR": "127.0.0.1:9090", "SWITCHYARD_HTTP_URL": base}

    def http_variant(user):
        request = urllib.request.Request(base + "/v1/evaluate", data=json.dumps({
            "project_id": project["id"], "environment_id": env["development"], "key": "listing_flow",
            "user_id": user, "fallback": safe}).encode(),
            headers={"Authorization": "Bearer " + key["token"], "Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=5) as response:
            return json.load(response)

    for user in users:
        code, out, err = run_example({**common, "SWITCHYARD_USER_ID": user})
        assert code == 0, (out, err)
        sdk_variant = re.search(r'variant="([^"]*)"', out).group(1)
        assert sdk_variant == http_variant(user)["variant_id"], (user, out)  # transport parity
        assert "accepted=3 quarantined=0 duplicates=0 rejected=0 retained=0" in out, out

    deadline = time.monotonic() + 30
    while True:
        status, data = admin.call("GET", path + "/experiments/" + run["id"] + "/results")
        assert status == 200
        processing = data["processing"]
        if processing["pending_events"] == 0 and processing["due_users"] == 0:
            break
        assert time.monotonic() < deadline, "measurement did not converge"
        time.sleep(0.2)
    assert sum(v["total"]["exposed"] for v in data["variants"]) == len(users)
    assert sum(v["total"]["converted"] for v in data["variants"]) == len(users)
    assert sum(v["requests"]["count"] for v in data["variants"]) == len(users)
    assert data["quality"]["quarantined_events"] == 0

    # A revoked key must degrade to the declared safe value, never to a stale or invented decision.
    assert admin.call("DELETE", path + "/application-keys/" + key["id"])[0] == 204
    code, out, err = run_example({**common, "SWITCHYARD_USER_ID": users[0]})
    assert "evaluation degraded" in out and "flow=control" in out and 'run=""' in out, (out, err)
    print("sdk drill passed:", len(users), "users, parity with HTTP, exact counts, revoked-key fallback")
    assert admin.call("DELETE", "/v1/session")[0] == 204


if __name__ == "__main__":
    main()
