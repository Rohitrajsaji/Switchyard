"""Real HTTP experiment lifecycle with independently computed Python assignments."""
import hashlib
import json
import struct
import urllib.error
import urllib.request
import uuid

from management_smoke import Client, base


def main():
    admin = Client("admin@example.test")
    status, project = admin.call("POST", "/v1/projects", {"name": "Experiments " + uuid.uuid4().hex[:8]})
    assert status == 201
    path = "/v1/projects/" + project["id"]
    status, environments = admin.call("GET", path + "/environments")
    assert status == 200
    env = next(item["id"] for item in environments if item["name"] == "development")
    safe = {"type": "boolean", "data": False}
    treatment = {"type": "boolean", "data": True}
    status, definition = admin.call("POST", path + "/flags", {
        "environment_id": env, "key": "listing", "type": "boolean", "default": safe, "safe": safe,
        "rules": [{"attribute": "country", "operator": "eq", "values": ["JP"], "value": treatment}],
        "reason": "listing experiment baseline",
    })
    assert status == 201
    status, key = admin.call("POST", path + "/application-keys", {
        "name": "Experiment smoke", "environment_id": env, "permissions": ["evaluate"]
    })
    assert status == 201
    status, run = admin.call("POST", path + "/experiments", {
        "environment_id": env, "flag_key": "listing", "expected_revision": 1,
        "name": "Simpler listing", "control_variant_id": "control", "traffic_bp": 10000,
        "variants": [
            {"id": "control", "ordinal": 0, "weight_bp": 3000, "value": safe},
            {"id": "treatment", "ordinal": 1, "weight_bp": 7000, "value": treatment},
        ], "reason": "measure completion",
    })
    assert status == 201 and run["state"] == "draft" and run["configuration_revision"] == 1
    frozen = run["definition"]
    run_path = path + "/experiments/" + run["id"]

    def transition(action, revision):
        status, result = admin.call("POST", run_path + "/transitions", {
            "action": action, "expected_revision": revision, "reason": "live lifecycle smoke"
        })
        assert status == 200, (action, status)
        assert result["configuration_revision"] == revision + 1 and result["definition"] == frozen
        return result

    def evaluate(user, attributes=None):
        request = urllib.request.Request(base + "/v1/evaluate", data=json.dumps({
            "project_id": project["id"], "environment_id": env, "key": "listing",
            "user_id": user, "attributes": attributes or {}, "fallback": safe,
        }).encode(), headers={"Authorization": "Bearer " + key["token"], "Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=5) as response:
            assert response.status == 200
            return json.load(response)

    def expected_variant(user):
        assignment = frozen["experiment"]
        fields = ["v1", project["id"], env, definition["flag_id"], run["id"],
                  "variant", assignment["variant_salt"], user]
        encoded = b"".join(struct.pack(">I", len(field.encode())) + field.encode() for field in fields)
        bucket = int.from_bytes(hashlib.sha256(encoded).digest()[:8], "big") % 10000
        return "control" if bucket < 3000 else "treatment"

    assert evaluate("synthetic-0")["reason"] == "default"
    transition("start", 1)
    for i in range(100):
        user = f"synthetic-{i}"
        decision = evaluate(user)
        assert decision["run_id"] == run["id"] and decision["variant_id"] == expected_variant(user)
        assert decision["value"]["data"] == (decision["variant_id"] == "treatment")
    override = evaluate("targeted", {"country": "JP"})
    assert override["reason"] == "targeting" and "run_id" not in override
    assert admin.call("POST", run_path + "/transitions", {
        "action": "pause", "expected_revision": 1, "reason": "stale request"
    })[0] == 409
    transition("pause", 2)
    assert evaluate("synthetic-0")["reason"] == "default"
    transition("start", 3)
    for i in range(100):
        assert evaluate(f"synthetic-{i}")["variant_id"] == expected_variant(f"synthetic-{i}")
    assert transition("complete", 4)["state"] == "completed"
    assert evaluate("synthetic-0")["reason"] == "default"
    status, historical = admin.call("GET", run_path)
    assert status == 200 and historical["definition"] == frozen and historical["completed_at"]
    status, listed = admin.call("GET", path + "/experiments?environment_id=" + env)
    assert status == 200 and listed[0]["id"] == run["id"]
    assert admin.call("DELETE", path + "/application-keys/" + key["id"])[0] == 204
    assert admin.call("DELETE", "/v1/session")[0] == 204
    print("Live experiments passed: unequal Python buckets, targeting exclusion, pause/resume stability, revision conflict and history")


if __name__ == "__main__":
    main()
