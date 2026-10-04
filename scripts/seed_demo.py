"""Opt-in synthetic marketplace fixture using authoritative Go management/events APIs.

Repeat runs reuse a cached project marker and immutable event identities. Existing
human changes are never reset, runs never restarted, and credentials never saved.
"""
import datetime
import fcntl
import hashlib
import json
import os
from pathlib import Path
import struct
import urllib.parse
import urllib.request

from management_smoke import Client, base

NAME = "Marketplace demo"
RUN_NAME = "Simplified listing flow · synthetic fixture"
SAFE = {"type": "boolean", "data": False}


def expect(client, method, path, body=None, statuses=(200,)):
    status, data = client.call(method, path, body)
    if status not in statuses:
        raise RuntimeError(f"Demo bootstrap {method} {path}: HTTP {status}")
    return data


def bucket(fields):
    encoded = b"".join(struct.pack(">I", len(f.encode())) + f.encode() for f in fields)
    return int.from_bytes(hashlib.sha256(encoded).digest()[:8], "big") % 10000


def fixture(run):
    """Stable imported facts, not claims about real UI usage or request latency."""
    d = run["definition"]
    experiment = d["experiment"]
    if experiment["traffic_bp"] != 10000 or d["rules"] or d.get("rollout"):
        raise RuntimeError("Existing demo population differs; refusing to rewrite it")
    counts = {v["id"]: {"exposed": 0, "converted": 0} for v in experiment["variants"]}
    events = []
    timestamp = run["started_at"]
    prefix = hashlib.sha256(run["id"].encode()).hexdigest()[:12]
    for i in range(100):
        user = f"synthetic_seed_{prefix}_{i}"
        assignment = bucket(["v1", d["project_id"], d["environment_id"], d["flag_id"], run["id"],
                             "variant", experiment["variant_salt"], user])
        cumulative = 0
        variant = None
        for item in sorted(experiment["variants"], key=lambda v: v["ordinal"]):
            cumulative += item["weight_bp"]
            if assignment < cumulative:
                variant = item["id"]
                break
        if variant is None:
            raise RuntimeError("Invalid stored allocation")
        exposure_id = f"seed_{prefix}_exposure_{i}"
        common = {"run_id": run["id"], "user_id": user, "variant_id": variant,
                  "revision": d["revision"] + 1, "decision_reason": "experiment",
                  "occurred_at": timestamp,
                  "attributes": {"country": "JP", "device": "desktop", "synthetic_seed": True}}
        events.append({**common, "event_id": exposure_id, "kind": "exposure",
                       "decision_id": f"seed_dec_{prefix}_{i}"})
        counts[variant]["exposed"] += 1
        if i % 2 == 0:
            events.append({**common, "event_id": f"seed_{prefix}_completion_{i}",
                           "kind": "listing_completion", "exposure_id": exposure_id})
            counts[variant]["converted"] += 1
    return events, counts


def bootstrap(admin, marker, save):
    project = marker.get("project")
    if project:
        status, _ = admin.call("GET", f"/v1/projects/{project['id']}/environments")
        if status == 403:  # A fresh database has no cached project identity.
            project = None
        elif status != 200:
            raise RuntimeError(f"Cannot read cached demo project: HTTP {status}")
    if not project:
        projects = expect(admin, "GET", "/v1/projects")
        matches = [p for p in projects if p["name"] == NAME]
        if not matches and len(projects) >= 100:
            raise RuntimeError("Project list is truncated; refusing to create a potentially duplicate fixture")
        if len(matches) > 1:
            raise RuntimeError("Multiple demo projects exist; refusing an ambiguous bootstrap")
        project = matches[0] if matches else expect(admin, "POST", "/v1/projects", {"name": NAME}, (201,))
        marker.clear()
        marker["project"] = project
        save(marker)
    path = "/v1/projects/" + project["id"]
    for user in ["demo_reviewer", "demo_developer", "demo_viewer"]:
        expect(admin, "POST", path + "/members", {"user_id": user}, (204, 409))
    environments = {e["name"]: e["id"] for e in expect(admin, "GET", path + "/environments")}
    for name in ["development", "staging"]:
        env = environments[name]
        definitions = expect(admin, "GET", path + "/flags?environment_id=" + env)
        if not any(d["key"] == "listing_flow" for d in definitions):
            configuration = {"environment_id": env, "default": SAFE, "safe": SAFE,
                             "rules": [], "reason": "Explicit synthetic marketplace bootstrap"}
            status, _ = admin.call("POST", path + "/flags", {**configuration, "key": "listing_flow", "type": "boolean"})
            if status == 409:
                # Keys belong to the project; another environment already owns
                # this key. Revision zero creates only the missing environment config.
                expect(admin, "PUT", path + "/flags/listing_flow", {**configuration, "expected_revision": 0})
            elif status != 201:
                raise RuntimeError(f"Cannot create demo flag: HTTP {status}")
        if name == "development" and not any(d["key"] == "listing_config" for d in definitions):
            classic = {"type": "json", "data": {"flow": "classic", "max_images": 4}}
            simple = {"type": "json", "data": {"flow": "simple", "max_images": 4}}
            expect(admin, "POST", path + "/flags", {"environment_id": env, "key": "listing_config", "type": "json",
                   "default": classic, "safe": classic, "rules": [], "rollout": {"traffic_bp": 1000, "value": simple},
                   "reason": "JSON configuration and 10% gradual rollout example"}, (201,))
    env = environments["development"]
    runs = expect(admin, "GET", path + "/experiments?environment_id=" + env)
    matches = [r for r in runs if r["name"] == RUN_NAME and r["definition"]["key"] == "listing_flow"]
    if len(matches) > 1:
        raise RuntimeError("Multiple fixture runs exist; refusing to modify them")
    if matches:
        run = matches[0]
    else:
        d = expect(admin, "GET", path + "/flags/listing_flow?environment_id=" + env)
        run = expect(admin, "POST", path + "/experiments", {"environment_id": env, "flag_key": "listing_flow",
                     "expected_revision": d["revision"], "name": RUN_NAME, "control_variant_id": "control", "traffic_bp": 10000,
                     "variants": [{"id": "control", "ordinal": 0, "weight_bp": 5000, "value": SAFE},
                                  {"id": "treatment", "ordinal": 1, "weight_bp": 5000, "value": {"type": "boolean", "data": True}}],
                     "reason": "Synthetic imported exposures/conversions; not real user behavior"}, (201,))
    if run["state"] == "draft" and not marker.get("measurement_complete"):
        run = expect(admin, "POST", path + "/experiments/" + run["id"] + "/transitions",
                     {"action": "start", "expected_revision": run["configuration_revision"], "reason": "Start the explicit local fixture"})
    if not run.get("started_at"):
        raise RuntimeError("Demo run was closed without starting; refusing to restart it")
    events, expected = fixture(run)
    if not marker.get("measurement_complete"):
        started = datetime.datetime.fromisoformat(run["started_at"].replace("Z", "+00:00"))
        if datetime.datetime.now(datetime.timezone.utc) - started > datetime.timedelta(hours=24):
            raise RuntimeError("Fixture facts are outside the lateness window; use a fresh local database")
        credential = expect(admin, "POST", path + "/application-keys", {"environment_id": env,
                            "name": "Synthetic fixture import", "permissions": ["events:write"]}, (201,))
        try:
            for offset in range(0, len(events), 100):
                batch = events[offset:offset + 100]
                request = urllib.request.Request(base + "/v1/events", data=json.dumps({"project_id": project["id"],
                    "environment_id": env, "events": batch}).encode(), headers={"Content-Type": "application/json",
                    "Authorization": "Bearer " + credential["token"]}, method="POST")
                with urllib.request.urlopen(request, timeout=10) as response:
                    receipts = json.load(response)["receipts"]
                if len(receipts) != len(batch) or any(r["event_id"] != e["event_id"] or r["status"] != "accepted" for r, e in zip(receipts, batch)):
                    raise RuntimeError("Fixture batch was not accepted; no successful import is claimed")
        finally:
            expect(admin, "DELETE", path + "/application-keys/" + credential["id"], statuses=(204, 409))
        marker["measurement_complete"] = True
        marker["run_id"] = run["id"]
        save(marker)
    result = expect(admin, "GET", path + "/experiments/" + run["id"] + "/results")
    for variant in result["variants"]:
        if any(variant["total"][key] < expected[variant["id"]][key] for key in ["exposed", "converted"]):
            raise RuntimeError("Results did not reconcile with the committed fixture")
    print("Marketplace demo ready: 100 synthetic exposures / 50 synthetic completions imported; no request-latency samples.")
    print("Project:", project["name"], "Run state:", run["state"], "(existing human changes are preserved)")


def main():
    parsed = urllib.parse.urlparse(base)
    if parsed.hostname not in {"localhost", "127.0.0.1", "::1"}:
        raise RuntimeError("Demo seeding is limited to a local API")
    root = Path(__file__).resolve().parent.parent / ".cache"
    root.mkdir(exist_ok=True)
    marker_path = root / ("demo-seed-" + hashlib.sha256(base.encode()).hexdigest()[:12] + ".json")
    with marker_path.with_suffix(".lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        marker = json.loads(marker_path.read_text()) if marker_path.exists() else {}

        def save(data):
            temporary = marker_path.with_suffix(".tmp")
            temporary.write_text(json.dumps(data, indent=2) + "\n")
            temporary.replace(marker_path)

        admin = Client("admin@example.test")
        try:
            bootstrap(admin, marker, save)
        finally:
            expect(admin, "DELETE", "/v1/session", statuses=(204, 401))


if __name__ == "__main__":
    main()
