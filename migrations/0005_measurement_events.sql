CREATE TABLE raw_events (
    project_id text NOT NULL,
    environment_id text NOT NULL,
    event_id text NOT NULL CHECK(length(event_id) BETWEEN 1 AND 128),
    run_id text NOT NULL,
    user_id text NOT NULL CHECK(length(user_id) BETWEEN 1 AND 256),
    kind text NOT NULL CHECK(kind IN ('exposure','listing_completion','request_outcome')),
    variant_id text NOT NULL,
    revision bigint NOT NULL CHECK(revision>0),
    exposure_id text,
    occurred_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    status text NOT NULL CHECK(status IN ('accepted','quarantined')),
    quarantine_reason text NOT NULL,
    is_error boolean,
    latency_ms double precision CHECK(latency_ms BETWEEN 0 AND 60000),
    payload jsonb NOT NULL,
    application_key_id text NOT NULL REFERENCES application_keys(id),
    PRIMARY KEY(project_id,environment_id,event_id),
    FOREIGN KEY(project_id,environment_id,run_id) REFERENCES experiment_runs(project_id,environment_id,id),
    CHECK((status='accepted' AND quarantine_reason='') OR (status='quarantined' AND quarantine_reason<>'')),
    CHECK((kind='request_outcome' AND is_error IS NOT NULL AND latency_ms IS NOT NULL) OR
          (kind<>'request_outcome' AND is_error IS NULL AND latency_ms IS NULL)),
    CHECK((kind='exposure' AND exposure_id IS NULL) OR (kind<>'exposure' AND exposure_id IS NOT NULL))
);
CREATE INDEX raw_events_attribution_idx ON raw_events(project_id,environment_id,run_id,user_id,occurred_at,event_id)
WHERE status='accepted';
CREATE INDEX raw_events_retention_idx ON raw_events(received_at);
CREATE INDEX raw_events_quarantine_idx ON raw_events(project_id,environment_id,run_id,received_at)
WHERE status='quarantined';
CREATE FUNCTION prevent_raw_event_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'raw event facts are immutable'; END;
$$;
CREATE TRIGGER raw_event_no_rewrite BEFORE UPDATE ON raw_events
FOR EACH ROW EXECUTE FUNCTION prevent_raw_event_rewrite();
