-- Copy only at raw folding, not on every ingestion. The normalized payload is
-- needed for PostgreSQL's exact semantic jsonb equality on retries. This keeps
-- identity/receipt data for eight days even though analytical raw retention is
-- seven days. Bounded expiry cleanup follows with the retention worker.
CREATE TABLE retained_event_identities (
 project_id text NOT NULL,
 environment_id text NOT NULL,
 event_id text NOT NULL,
 payload jsonb NOT NULL CHECK(jsonb_typeof(payload)='object'),
 status text NOT NULL CHECK(status IN ('accepted','quarantined')),
 quarantine_reason text NOT NULL,
 received_at timestamptz NOT NULL,
 expires_at timestamptz NOT NULL,
 PRIMARY KEY(project_id,environment_id,event_id),
 FOREIGN KEY(project_id,environment_id) REFERENCES environments(project_id,id),
 CHECK(expires_at=received_at+interval '192 hours'),
 CHECK((status='accepted' AND quarantine_reason='') OR (status='quarantined' AND quarantine_reason<>''))
);
CREATE INDEX retained_identity_expiry_idx ON retained_event_identities(expires_at,project_id,environment_id,event_id);
CREATE TRIGGER retained_identity_no_rewrite BEFORE UPDATE ON retained_event_identities FOR EACH ROW EXECUTE FUNCTION prevent_raw_event_rewrite();
