-- Per-user reconciliation filters raw facts by project, environment, run and user.
-- The receipt-time index cannot serve that lookup: a null reporting floor used to
-- look like received_at >= -infinity and the generic plan walked every fact.
CREATE INDEX raw_events_user_receipt_idx ON raw_events(project_id, environment_id, run_id, user_id, received_at);
