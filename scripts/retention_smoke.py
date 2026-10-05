"""Verify opt-in loop startup/stop without cleaning normal demo source data."""
import datetime
import json
import pathlib
import subprocess
import time


def command(args):
    return subprocess.check_output(args, text=True, stderr=subprocess.PIPE).strip()


def sql(query):
    return command(["docker", "compose", "exec", "-T", "postgres", "psql", "-U",
                    "switchyard", "-d", "switchyard", "-At", "-c", query])


def counts():
    return json.loads(sql("""SELECT json_build_object(
      'raw',(SELECT count(*) FROM raw_events),
      'history',(SELECT count(*) FROM metric_history_segments),
      'pending',(SELECT count(*) FROM metric_pending_outcomes),
      'identities',(SELECT count(*) FROM retained_event_identities),
      'publications',(SELECT count(*) FROM outbox),
      'receipts',(SELECT count(*) FROM processed_work))"""))


def logs():
    lines = command(["docker", "compose", "logs", "--no-log-prefix", "--tail=100", "worker"]).splitlines()
    result = []
    for line in lines:
        try:
            result.append(json.loads(line))
        except json.JSONDecodeError:
            pass
    return result


settings = json.loads(command(["docker", "compose", "--profile", "async", "config", "--format", "json"]))["services"]["worker"]["environment"]
assert settings["WORKER_RETENTION_ENABLED"].lower() in ("false", "0"), "Diagnostic requires normal retention disabled"
raw_days = int(settings["RAW_RETENTION_DAYS"])
assert 2 <= raw_days <= 7
before = counts()
assert before["history"] == before["pending"] == before["identities"] == 0, "Use disposable integration fixtures for aged data"
assert sql(f"""SELECT NOT EXISTS(SELECT 1 FROM raw_events WHERE received_at<now()-interval '{raw_days} days')
 AND NOT EXISTS(SELECT 1 FROM outbox WHERE created_at<now()-interval '8 days')
 AND NOT EXISTS(SELECT 1 FROM processed_work WHERE processed_at<now()-interval '8 days')
 AND NOT EXISTS(SELECT 1 FROM work_dead_letters WHERE resolved_at<now()-interval '8 days')""") == "t", "Diagnostic refuses cleanable normal data"
override = pathlib.Path(".cache/retention-smoke.override.yaml")
override.parent.mkdir(parents=True, exist_ok=True)
override.write_text('services:\n  worker:\n    environment:\n      WORKER_RETENTION_ENABLED: "true"\n')
observed = None
try:
    command(["docker", "compose", "-f", "compose.yaml", "-f", str(override), "--profile", "cache", "--profile", "async", "up", "--no-build", "-d", "--wait", "worker"])
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        candidates = [entry for entry in logs() if entry.get("msg") == "retention statistics"]
        if candidates:
            observed = candidates[-1]
            break
        time.sleep(0.25)
    assert observed is not None, "Enabled loop did not report a cycle"
    assert observed["failed_cycles"] == 0 and all(value == 0 for value in observed["statistics"].values()), observed
    assert counts() == before, "Diagnostic unexpectedly changed retained sources"
finally:
    command(["docker", "compose", "--profile", "cache", "--profile", "async", "up", "--no-build", "-d", "--wait", "worker"])
    override.unlink(missing_ok=True)
deadline = time.monotonic() + 15
started = []
while time.monotonic() < deadline:
    started = [entry for entry in logs() if entry.get("msg") == "worker started"]
    if started:
        break
    time.sleep(0.25)
assert started and started[-1]["processing_enabled"] and not started[-1]["retention_enabled"], "Normal processing/retention settings not restored"
report = {"observed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
          "raw_horizon_days": raw_days, "source_counts": before,
          "enabled_loop": observed, "restored_retention_enabled": False,
          "scope": "Fresh local sources; real aged deletion/replay/expiry verified in disposable integration schemas."}
pathlib.Path(".cache/retention-smoke-report.json").write_text(json.dumps(report, indent=2) + "\n")
print(json.dumps(report, indent=2))
