"""Live flag journey; independently recompute bucket membership in Python."""
import hashlib
import json
import struct
import urllib.error
import urllib.request
import uuid

from management_smoke import Client, base

admin = Client("admin@example.test")
status, project = admin.call("POST", "/v1/projects", {"name": "Flags " + uuid.uuid4().hex[:8]})
assert status == 201
path = "/v1/projects/" + project["id"]
status, environments = admin.call("GET", path + "/environments")
assert status == 200
env = {item["name"]: item["id"] for item in environments}
status, key = admin.call("POST", path + "/application-keys", {
    "name": "Flag smoke", "environment_id": env["development"], "permissions": ["evaluate"]
})
assert status == 201


def evaluate(flag, user="user-123", attributes=None, fallback=None):
    body = {
        "project_id": project["id"], "environment_id": env["development"],
        "key": flag, "user_id": user, "attributes": attributes or {},
        "fallback": fallback or {"type": "boolean", "data": False},
    }
    request = urllib.request.Request(base + "/v1/evaluate", data=json.dumps(body).encode(), headers={
        "Authorization": "Bearer " + key["token"], "Content-Type": "application/json"
    })
    try:
        response = urllib.request.urlopen(request, timeout=5)
    except urllib.error.HTTPError as e:
        response = e
    with response:
        return response.status, json.load(response)


configuration = {
    "default": {"type": "boolean", "data": False}, "safe": {"type": "boolean", "data": False},
    "rules": [{"attribute": "country", "operator": "eq", "values": ["JP"], "value": {"type": "boolean", "data": True}}],
    "rollout": {"traffic_bp": 1000, "value": {"type": "boolean", "data": True}},
}
status, definition = admin.call("POST", path + "/flags", {
    **configuration, "environment_id": env["development"], "key": "listing", "type": "boolean", "reason": "live smoke"
})
assert status == 201 and definition["revision"] == 1
status, decision = evaluate("listing", attributes={"country": "JP"})
assert status == 200 and decision["reason"] == "targeting" and decision["value"]["data"] is True
assert decision["decision_id"] and "run_id" not in decision
assert evaluate("listing", fallback={"type": "boolean", "data": True})[0] == 400


def expected(user, traffic):
    fields = ["v1", project["id"], env["development"], definition["flag_id"],
              "rollout:standalone-v1", "eligibility", "standalone-v1", user]
    data = b"".join(struct.pack(">I", len(s.encode())) + s.encode() for s in fields)
    return int.from_bytes(hashlib.sha256(data).digest()[:8], "big") % 10000 < traffic


small = set()
for i in range(100):
    user = f"synthetic-{i}"
    status, result = evaluate("listing", user=user)
    assert status == 200 and result["value"]["data"] == expected(user, 1000)
    if result["value"]["data"]:
        small.add(user)
configuration["rollout"]["traffic_bp"] = 2000
update = {**configuration, "environment_id": env["development"], "expected_revision": 1, "reason": "grow traffic"}
assert admin.call("PUT", path + "/flags/listing", update)[0] == 200
assert admin.call("PUT", path + "/flags/listing", update)[0] == 409
for i in range(100):
    user = f"synthetic-{i}"
    status, result = evaluate("listing", user=user)
    assert status == 200 and result["revision"] == 2 and result["value"]["data"] == expected(user, 2000)
    if user in small:
        assert result["value"]["data"]
update.update(expected_revision=2, killed=True, reason="safe disable")
assert admin.call("PUT", path + "/flags/listing", update)[0] == 200
status, result = evaluate("listing", attributes={"country": "JP"})
assert status == 200 and result["reason"] == "kill_switch" and result["value"]["data"] is False

status, _ = admin.call("POST", path + "/flags", {
    "environment_id": env["development"], "key": "json_flow", "type": "json", "killed": True,
    "default": {"type": "json", "data": {"flow": "treatment"}},
    "safe": {"type": "json", "data": {"flow": "control", "timeout": 100.0}}, "reason": "JSON safe value"
})
assert status == 201
status, result = evaluate("json_flow", fallback={"type": "json", "data": {"timeout": 1e2, "flow": "control"}})
assert status == 200 and result["value"]["data"]["flow"] == "control"
assert admin.call("DELETE", path + "/application-keys/" + key["id"])[0] == 204
assert evaluate("listing")[0] == 401
assert admin.call("DELETE", "/v1/session")[0] == 204
print("Live flags passed: independent Python buckets, stable growth, conflict, targeting, boolean/JSON safety, key revoke")
