"""Live production approval journey: central policy, exact-diff approval, separation of duties,
idempotent apply, stale detection and emergency exemptions. Requires seeded demo credentials."""
import uuid

from management_smoke import Client

SAFE = {"type": "boolean", "data": False}
ON = {"type": "boolean", "data": True}


def rollout(bp, killed=False):
    return {"default": SAFE, "safe": SAFE, "killed": killed, "rules": [],
            "rollout": {"traffic_bp": bp, "value": ON}}


def main():
    admin = Client("admin@example.test")
    reviewer = Client("reviewer@example.test")
    developer = Client("developer@example.test")
    viewer = Client("viewer@example.test")
    status, project = admin.call("POST", "/v1/projects", {"name": "Approval " + uuid.uuid4().hex[:8]})
    assert status == 201
    path = "/v1/projects/" + project["id"]
    for user in ("demo_reviewer", "demo_developer", "demo_viewer"):
        assert admin.call("POST", path + "/members", {"user_id": user})[0] == 204
    status, environments = admin.call("GET", path + "/environments")
    prod = next(e["id"] for e in environments if e["name"] == "production")

    def propose(client, key, kind, base, config, **extra):
        return client.call("POST", path + "/proposals", {
            "environment_id": prod, "key": key, "kind": kind, "expected_revision": base,
            "rationale": "approval drill", **config, **extra})

    # Direct production edits stay closed, except emergency reductions.
    assert admin.call("POST", path + "/flags", {"environment_id": prod, "key": "listing", "type": "boolean",
        "reason": "bypass", **rollout(1000)})[0] == 403
    status, p = propose(developer, "listing", "create", 0, rollout(1000), type="boolean")
    assert status == 201 and p["state"] == "validated" and len(p["diff_hash"]) == 64, p
    assert propose(viewer, "listing", "create", 0, rollout(1000), type="boolean")[0] == 403
    assert admin.call("POST", f"{path}/proposals/{p['id']}/apply")[0] == 409  # not approved
    assert developer.call("POST", f"{path}/proposals/{p['id']}/approve", {"diff_hash": p["diff_hash"], "reason": "self"})[0] == 403
    assert reviewer.call("POST", f"{path}/proposals/{p['id']}/approve", {"diff_hash": "0" * 64, "reason": "wrong diff"})[0] == 409
    status, approved = reviewer.call("POST", f"{path}/proposals/{p['id']}/approve", {"diff_hash": p["diff_hash"], "reason": "reviewed diff"})
    assert status == 200 and approved["state"] == "approved" and approved["approver_id"] == "demo_reviewer"
    status, applied = developer.call("POST", f"{path}/proposals/{p['id']}/apply")
    assert status == 200 and applied["state"] == "applied" and applied["applied_revision"] == 1, applied
    status, again = developer.call("POST", f"{path}/proposals/{p['id']}/apply")
    assert status == 200 and again["applied_revision"] == 1  # idempotent

    status, flag = admin.call("GET", f"{path}/flags/listing?environment_id={prod}")
    assert status == 200 and flag["revision"] == 1 and flag["rollout"]["traffic_bp"] == 1000

    # Raising traffic directly is denied; as a reviewed proposal it succeeds; a stale sibling fails.
    raise_direct = {"environment_id": prod, "expected_revision": 1, "reason": "direct", **rollout(2000)}
    assert admin.call("PUT", path + "/flags/listing", raise_direct)[0] == 403
    status, up = propose(developer, "listing", "update", 1, rollout(2000))
    assert status == 201
    status, sibling = propose(developer, "listing", "update", 1, rollout(1500))
    assert status == 201
    assert reviewer.call("POST", f"{path}/proposals/{up['id']}/approve", {"diff_hash": up["diff_hash"], "reason": "ok"})[0] == 200
    assert reviewer.call("POST", f"{path}/proposals/{sibling['id']}/approve", {"diff_hash": sibling["diff_hash"], "reason": "ok"})[0] == 200
    assert developer.call("POST", f"{path}/proposals/{up['id']}/apply")[0] == 200
    status, body = developer.call("POST", f"{path}/proposals/{sibling['id']}/apply")
    assert status == 409 and body["error"] == "stale", body
    assert developer.call("GET", f"{path}/proposals/{sibling['id']}")[1]["state"] == "stale"

    # Emergency reductions need no review; re-enabling does.
    status, reduced = developer.call("PUT", path + "/flags/listing", {"environment_id": prod, "expected_revision": 2, "reason": "emergency reduce", **rollout(500)})
    assert status == 200 and reduced["revision"] == 3
    status, killed = developer.call("PUT", path + "/flags/listing", {"environment_id": prod, "expected_revision": 3, "reason": "emergency kill", **rollout(500, killed=True)})
    assert status == 200 and killed["killed"] is True
    assert developer.call("PUT", path + "/flags/listing", {"environment_id": prod, "expected_revision": 4, "reason": "re-enable", **rollout(500)})[0] == 403

    status, entries = admin.call("GET", path + "/audit")
    actions = [e["action"] for e in entries]
    for expected in ("proposal.created", "proposal.approved", "proposal.applied", "proposal.stale", "flag.updated"):
        assert expected in actions, (expected, actions)
    assert actions.count("proposal.applied") == 2
    print("approval drill passed: policy gate, separation of duties, exact diff, idempotent apply, stale, emergency exemptions")
    assert admin.call("DELETE", "/v1/session")[0] == 204


if __name__ == "__main__":
    main()
