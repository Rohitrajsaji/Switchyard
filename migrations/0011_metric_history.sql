-- Retention foundation: historical contributions are separate from the
-- replaceable raw-retained contribution. Cleanup is added after parity gates.
ALTER TABLE metric_user_state ADD COLUMN historical_contribution jsonb NOT NULL DEFAULT '{}';
CREATE TABLE metric_archived_anchors (
 project_id text NOT NULL,
 environment_id text NOT NULL,
 run_id text NOT NULL,
 user_id text NOT NULL,
 event_id text NOT NULL,
 variant_id text NOT NULL,
 occurred_at timestamptz NOT NULL,
 PRIMARY KEY(project_id,environment_id,run_id,user_id),
 FOREIGN KEY(project_id,environment_id,run_id,user_id) REFERENCES metric_user_state(project_id,environment_id,run_id,user_id)
);
-- No attributes, original payload, application credential or latency samples.
-- These projections allow retained outcomes to validate a purged reference.
CREATE TABLE metric_event_references (
 project_id text NOT NULL,
 environment_id text NOT NULL,
 event_id text NOT NULL,
 run_id text NOT NULL,
 user_id text NOT NULL,
 kind text NOT NULL CHECK(kind IN ('exposure','listing_completion','request_outcome')),
 status text NOT NULL CHECK(status IN ('accepted','quarantined')),
 variant_id text NOT NULL,
 occurred_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL,
 PRIMARY KEY(project_id,environment_id,event_id),
 FOREIGN KEY(project_id,environment_id,run_id) REFERENCES experiment_runs(project_id,environment_id,id)
);
CREATE INDEX metric_reference_expiry_idx ON metric_event_references(received_at);
