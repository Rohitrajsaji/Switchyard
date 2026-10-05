ALTER TABLE metric_archived_anchors ADD COLUMN received_at timestamptz;
UPDATE metric_archived_anchors SET received_at=occurred_at;
ALTER TABLE metric_archived_anchors ALTER COLUMN received_at SET NOT NULL;
ALTER TABLE metric_archived_anchors ADD COLUMN converted boolean NOT NULL DEFAULT false;
CREATE TABLE metric_pending_outcomes (
 project_id text NOT NULL,environment_id text NOT NULL,event_id text NOT NULL,
 run_id text NOT NULL,user_id text NOT NULL,kind text NOT NULL CHECK(kind IN ('listing_completion','request_outcome')),
 variant_id text NOT NULL,revision bigint NOT NULL,exposure_id text NOT NULL,
 occurred_at timestamptz NOT NULL,received_at timestamptz NOT NULL,
 status text NOT NULL CHECK(status='accepted'),quarantine_reason text NOT NULL CHECK(quarantine_reason=''),
 is_error boolean,latency_ms double precision,
 PRIMARY KEY(project_id,environment_id,event_id),
 FOREIGN KEY(project_id,environment_id,run_id,user_id) REFERENCES metric_user_state(project_id,environment_id,run_id,user_id),
 CHECK(length(event_id) BETWEEN 1 AND 128 AND length(exposure_id) BETWEEN 1 AND 128 AND length(user_id) BETWEEN 1 AND 256),
 CHECK(revision>0),CHECK(latency_ms BETWEEN 0 AND 60000),
 CHECK((kind='request_outcome' AND is_error IS NOT NULL AND latency_ms IS NOT NULL) OR
 (kind='listing_completion' AND is_error IS NULL AND latency_ms IS NULL))
);
CREATE INDEX metric_pending_reference_idx ON metric_pending_outcomes(project_id,environment_id,exposure_id);
CREATE INDEX metric_pending_user_idx ON metric_pending_outcomes(project_id,environment_id,run_id,user_id,received_at,event_id);
-- Each segment owns disjoint source facts, grouped by original UTC receipt day.
-- Cohort identity is stored separately in archived anchors, not added per day.
CREATE TABLE metric_history_segments (
 project_id text NOT NULL,environment_id text NOT NULL,run_id text NOT NULL,user_id text NOT NULL,
 receipt_day date NOT NULL,contribution jsonb NOT NULL,
 PRIMARY KEY(project_id,environment_id,run_id,user_id,receipt_day),
 FOREIGN KEY(project_id,environment_id,run_id,user_id) REFERENCES metric_user_state(project_id,environment_id,run_id,user_id)
);
CREATE INDEX metric_history_expiry_idx ON metric_history_segments(receipt_day,project_id,environment_id,run_id,user_id);
