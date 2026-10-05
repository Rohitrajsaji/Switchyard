-- Reviewed production configuration proposals. Content is immutable once stored; only the
-- review/application columns move, along a fixed state graph.
CREATE TABLE proposals (
    id text PRIMARY KEY,
    project_id text NOT NULL,
    environment_id text NOT NULL,
    flag_key text NOT NULL CHECK (length(flag_key) BETWEEN 1 AND 128),
    kind text NOT NULL CHECK (kind IN ('create','update')),
    flag_type text NOT NULL CHECK (flag_type IN ('boolean','json')),
    base_revision bigint NOT NULL CHECK (base_revision >= 0),
    configuration jsonb NOT NULL,
    diff jsonb NOT NULL,
    diff_hash text NOT NULL CHECK (diff_hash ~ '^[0-9a-f]{64}$'),
    rationale text NOT NULL CHECK (length(rationale) BETWEEN 1 AND 512),
    proposer_id text NOT NULL REFERENCES users(id),
    source text NOT NULL CHECK (source IN ('human','agent')),
    state text NOT NULL CHECK (state IN ('validated','approved','applied','rejected','expired','stale')),
    approver_id text REFERENCES users(id),
    approved_at timestamptz,
    approval_expires_at timestamptz,
    applied_revision bigint,
    created_at timestamptz NOT NULL,
    decided_at timestamptz,
    FOREIGN KEY (project_id, environment_id) REFERENCES environments(project_id, id),
    CHECK (state NOT IN ('approved','applied') OR (approver_id IS NOT NULL AND approver_id <> proposer_id AND approved_at IS NOT NULL AND approval_expires_at > approved_at)),
    CHECK (state <> 'applied' OR applied_revision IS NOT NULL)
);
CREATE INDEX proposals_project_idx ON proposals(project_id, created_at DESC, id);
CREATE INDEX proposals_open_idx ON proposals(project_id, environment_id, flag_key) WHERE state IN ('validated','approved');

CREATE FUNCTION proposals_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE','TRUNCATE') THEN
        RAISE EXCEPTION 'proposals are retained';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.environment_id IS DISTINCT FROM OLD.environment_id OR NEW.flag_key IS DISTINCT FROM OLD.flag_key
       OR NEW.kind IS DISTINCT FROM OLD.kind OR NEW.flag_type IS DISTINCT FROM OLD.flag_type
       OR NEW.base_revision IS DISTINCT FROM OLD.base_revision OR NEW.configuration IS DISTINCT FROM OLD.configuration
       OR NEW.diff IS DISTINCT FROM OLD.diff OR NEW.diff_hash IS DISTINCT FROM OLD.diff_hash
       OR NEW.rationale IS DISTINCT FROM OLD.rationale OR NEW.proposer_id IS DISTINCT FROM OLD.proposer_id
       OR NEW.source IS DISTINCT FROM OLD.source OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'proposal content is immutable';
    END IF;
    IF OLD.state IN ('applied','rejected','stale','expired') AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'proposal is final';
    END IF;
    IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
        (OLD.state = 'validated' AND NEW.state IN ('approved','rejected','stale')) OR
        (OLD.state = 'approved' AND NEW.state IN ('applied','rejected','stale','expired'))
    ) THEN
        RAISE EXCEPTION 'invalid proposal transition % to %', OLD.state, NEW.state;
    END IF;
    IF OLD.approver_id IS NOT NULL AND (NEW.approver_id IS DISTINCT FROM OLD.approver_id
       OR NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approval_expires_at IS DISTINCT FROM OLD.approval_expires_at) THEN
        RAISE EXCEPTION 'approval is immutable';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER proposals_guard BEFORE UPDATE OR DELETE ON proposals
    FOR EACH ROW EXECUTE FUNCTION proposals_guard();
CREATE TRIGGER proposals_no_truncate BEFORE TRUNCATE ON proposals
    FOR EACH STATEMENT EXECUTE FUNCTION proposals_guard();
