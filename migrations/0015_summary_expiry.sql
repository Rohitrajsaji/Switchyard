-- Monotonic reporting floor; anchor identity remains separate from its counts.
ALTER TABLE metric_user_state ADD COLUMN reporting_since timestamptz NOT NULL DEFAULT '-infinity';
CREATE INDEX metric_pending_expiry_idx ON metric_pending_outcomes(received_at,project_id,environment_id,run_id,user_id);
CREATE INDEX metric_archived_anchor_receipt_idx ON metric_archived_anchors(received_at,project_id,environment_id,run_id,user_id);
