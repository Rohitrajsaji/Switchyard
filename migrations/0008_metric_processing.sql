CREATE TABLE processed_work (
    message_id text PRIMARY KEY CHECK(length(message_id) BETWEEN 1 AND 128),
    reference jsonb NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE metric_user_state (
    project_id text NOT NULL,
    environment_id text NOT NULL,
    run_id text NOT NULL,
    user_id text NOT NULL CHECK(length(user_id) BETWEEN 1 AND 256),
    contribution jsonb NOT NULL DEFAULT '{}',
    reconciled_at timestamptz,
    due_at timestamptz,
    PRIMARY KEY(project_id,environment_id,run_id,user_id),
    FOREIGN KEY(project_id,environment_id,run_id) REFERENCES experiment_runs(project_id,environment_id,id)
);
CREATE INDEX metric_work_due_idx ON metric_user_state(due_at,project_id,environment_id,run_id,user_id) WHERE due_at IS NOT NULL;
CREATE INDEX raw_events_exposure_reference_idx ON raw_events(project_id,environment_id,exposure_id) WHERE exposure_id IS NOT NULL;
CREATE TABLE metric_counts (
    project_id text NOT NULL,
    environment_id text NOT NULL,
    run_id text NOT NULL,
    variant_id text NOT NULL,
    category text NOT NULL CHECK(category IN ('provisional','finalized','request','latency','quality')),
    metric text NOT NULL,
    bucket integer NOT NULL DEFAULT 0,
    value bigint NOT NULL CHECK(value>=0),
    PRIMARY KEY(project_id,environment_id,run_id,variant_id,category,metric,bucket),
    FOREIGN KEY(project_id,environment_id,run_id) REFERENCES experiment_runs(project_id,environment_id,id)
);
CREATE TABLE work_dead_letters (
    stream_name text NOT NULL,
    stream_sequence bigint NOT NULL CHECK(stream_sequence>0),
    message_id text NOT NULL,
    payload bytea NOT NULL CHECK(octet_length(payload)<=2048),
    failure_code text NOT NULL CHECK(failure_code IN ('invalid_envelope','identity_mismatch','source_missing')),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY(stream_name,stream_sequence)
);
