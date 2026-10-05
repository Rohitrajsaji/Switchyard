"""Create the synthetic benchmark fixture: 100 flags (10 running A/B experiments), one scoped
application key and a deterministic user pool whose experiment assignments are computed locally.

The fixture file contains a local application credential and is git-ignored. It is the only
output; nothing outside the local stack is touched."""
import json
import pathlib
import uuid

from management_smoke import Client
from ingestion_trial import bucket

ROOT = pathlib.Path(__file__).resolve().parent.parent
OUT = ROOT / "loadtest" / "fixture.json"
TRUE = {"type": "boolean", "data": True}
FALSE = {"type": "boolean", "data": False}
POOL = 20000


def flag(key, kind, **extra):
    return {"key": key, "kind": kind, **extra}


def main():
    admin = Client("admin@example.test")
    status, project = admin.call("POST", "/v1/projects", {"name": "Load " + uuid.uuid4().hex[:8]})
    assert status == 201
    path = "/v1/projects/" + project["id"]
    environments = admin.call("GET", path + "/environments")[1]
    dev = next(e["id"] for e in environments if e["name"] == "development")
    flags = []

    def create(key, body):
        status, response = admin.call("POST", path + "/flags", {"environment_id": dev, "key": key, "reason": "load fixture", **body})
        assert status == 201, (key, response)
        return response

    for n in range(100):
        key = f"load_{n:03d}"
        if n < 10:  # experiments attach to these after creation
            create(key, {"type": "boolean", "default": FALSE, "safe": FALSE})
            flags.append(flag(key, "experiment"))
        elif n < 50:
            create(key, {"type": "boolean", "default": TRUE, "safe": FALSE})
            flags.append(flag(key, "always_on"))
        elif n < 60:  # rule-heavy: nineteen non-matching rules, then the matching one
            rules = [{"attribute": "tier", "operator": "eq", "values": [f"decoy-{i}"], "value": TRUE} for i in range(19)]
            rules.append({"attribute": "country", "operator": "eq", "values": ["JP"], "value": TRUE})
            create(key, {"type": "boolean", "default": FALSE, "safe": FALSE, "rules": rules})
            flags.append(flag(key, "rules_country"))
        elif n < 70:
            create(key, {"type": "boolean", "default": FALSE, "safe": FALSE, "rules": [{"attribute": "age", "operator": "gte", "values": [18], "value": TRUE}]})
            flags.append(flag(key, "rules_age"))
        elif n < 80:
            payload = {"type": "json", "data": {"layout": "v2", "steps": [1, 2, 3]}}
            safe = {"type": "json", "data": {"layout": "v1"}}
            create(key, {"type": "json", "default": payload, "safe": safe})
            flags.append(flag(key, "json", expected=payload["data"], safe=safe))
        elif n < 90:
            create(key, {"type": "boolean", "default": TRUE, "safe": FALSE, "killed": True})
            flags.append(flag(key, "killed"))
        else:
            create(key, {"type": "boolean", "default": FALSE, "safe": FALSE, "rollout": {"traffic_bp": 5000, "value": TRUE}})
            flags.append(flag(key, "rollout"))

    runs = []
    for f in flags[:10]:
        status, run = admin.call("POST", path + "/experiments", {
            "environment_id": dev, "flag_key": f["key"], "expected_revision": 1, "name": "Load " + f["key"],
            "control_variant_id": "control", "traffic_bp": 10000,
            "variants": [{"id": "control", "ordinal": 0, "weight_bp": 5000, "value": FALSE},
                         {"id": "treatment", "ordinal": 1, "weight_bp": 5000, "value": TRUE}], "reason": "load fixture"})
        assert status == 201, run
        status, started = admin.call("POST", f"{path}/experiments/{run['id']}/transitions", {"action": "start", "expected_revision": 1, "reason": "load fixture"})
        assert status == 200, started
        f["run_id"] = run["id"]
        runs.append(run["id"])
    status, key = admin.call("POST", path + "/application-keys", {"name": "Load", "environment_id": dev, "permissions": ["evaluate", "events:write"]})
    assert status == 201

    # Deterministic user pool for the first experiment, assigned with the independent Python hash.
    first = flags[0]
    definition = admin.call("GET", f"{path}/flags/{first['key']}?environment_id={dev}")[1]
    users = []
    for n in range(POOL):
        user = f"load-user-{n}"
        users.append([user, bucket(definition, user)])
    fixture = {"project_id": project["id"], "environment_id": dev, "token": key["token"], "flags": flags,
               "run_id": first["run_id"], "revision": definition["revision"], "users": users}
    OUT.parent.mkdir(exist_ok=True)
    OUT.write_text(json.dumps(fixture))
    OUT.chmod(0o600)
    treatment = sum(1 for _, v in users if v == "treatment")
    print(f"fixture: {len(flags)} flags, {len(runs)} running experiments, pool of {POOL} users ({treatment} treatment), written to {OUT.relative_to(ROOT)}")
    assert admin.call("DELETE", "/v1/session")[0] == 204


if __name__ == "__main__":
    main()
