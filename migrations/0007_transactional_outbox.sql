-- Durable publication intent. The message contains references, not user attributes.
CREATE TABLE outbox (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind text NOT NULL CHECK(kind IN ('event','configuration')),
    project_id text NOT NULL CHECK(length(project_id) BETWEEN 1 AND 128),
    environment_id text NOT NULL CHECK(length(environment_id) BETWEEN 1 AND 128),
    object_id text NOT NULL CHECK(length(object_id) BETWEEN 1 AND 128),
    revision bigint NOT NULL CHECK(revision > 0),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    available_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    claim_token text,
    lease_until timestamptz,
    attempts integer NOT NULL DEFAULT 0 CHECK(attempts BETWEEN 0 AND 10),
    published_at timestamptz,
    dead_at timestamptz,
    failure_code text NOT NULL DEFAULT '' CHECK(failure_code IN ('','publish_failed','attempts_exhausted')),
    UNIQUE(kind,project_id,environment_id,object_id,revision),
    CHECK((claim_token IS NULL) = (lease_until IS NULL)),
    CHECK(published_at IS NULL OR dead_at IS NULL)
);
CREATE INDEX outbox_pending_idx ON outbox(available_at,id)
WHERE published_at IS NULL AND dead_at IS NULL;
CREATE INDEX outbox_dead_idx ON outbox(dead_at,id) WHERE dead_at IS NOT NULL;

-- Existing retained facts/configurations must participate when upgrading the MVP.
INSERT INTO outbox(kind,project_id,environment_id,object_id,revision)
SELECT 'event',project_id,environment_id,event_id,revision FROM raw_events;
INSERT INTO outbox(kind,project_id,environment_id,object_id,revision)
SELECT 'configuration',project_id,environment_id,flag_id,revision FROM flag_revisions;
