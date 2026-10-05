"""Exercise bounded, audited replay against existing local demo facts."""
import datetime
import json
import pathlib
import subprocess
import time


def command(args):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE).strip()


def sql(query):
    return command(["docker", "compose", "exec", "-T", "postgres", "psql",
                    "-U", "switchyard", "-d", "switchyard", "-At", "-c", query])


settings = json.loads(command(["docker", "compose", "--profile", "async", "config", "--format", "json"]))["services"]["worker"]["environment"]
raw_days = int(settings.get("RAW_RETENTION_DAYS", 7))
assert 2 <= raw_days <= 7
scope = json.loads(sql(f"""
SELECT row_to_json(s) FROM (
SELECT r.project_id AS project,r.environment_id AS environment,r.id AS run,u.id AS actor
FROM experiment_runs r JOIN environments e ON e.id=r.environment_id
JOIN project_memberships m ON m.project_id=r.project_id
JOIN users u ON u.id=m.user_id AND u.role='admin' AND u.active
WHERE e.name<>'production' AND EXISTS(SELECT 1 FROM raw_events f WHERE f.run_id=r.id
AND f.project_id=r.project_id AND f.received_at>=now()-interval '{raw_days} days')
ORDER BY r.created_at,r.id LIMIT 1) s
"""))
base = ["docker", "compose", "--profile", "async", "run", "--rm", "--no-deps",
        "--entrypoint", "/app/workctl", "worker", "-actor", scope["actor"],
        "-project", scope["project"], "-environment", scope["environment"]]
failures = json.loads(command(base + ["-action", "inspect"]))
now = datetime.datetime.now(datetime.timezone.utc)
start = (now - datetime.timedelta(days=raw_days) + datetime.timedelta(minutes=1)).isoformat()
end = now.isoformat()
pages, count, cursor = 0, 0, ""
while True:
    page = json.loads(command(base + ["-action", "replay", "-run", scope["run"],
                      "-from", start, "-until", end, "-cursor", cursor,
                      "-reason", "Verify local retained replay without resetting receipts"]))
    pages += 1
    count += page["events"]
    assert page["events"] <= 100
    if not page["more"]:
        break
    assert page["cursor"] > cursor and pages < 100
    cursor = page["cursor"]
assert count > 0
deadline = time.monotonic() + 30
while True:
    result = subprocess.run(["make", "aggregation-parity"], text=True, capture_output=True)
    if result.returncode == 0:
        break
    assert time.monotonic() < deadline, result.stderr + result.stdout
    time.sleep(0.25)
report = {"observed_at": now.isoformat(), "replayed_events": count, "pages": pages, "raw_horizon_days": raw_days,
          "inspected_failures": len(failures["failures"]), "parity": result.stdout.strip()}
report_path = pathlib.Path(".cache/recovery-smoke-report.json")
report_path.parent.mkdir(parents=True, exist_ok=True)
report_path.write_text(json.dumps(report, indent=2) + "\n")
print(json.dumps(report, indent=2))
