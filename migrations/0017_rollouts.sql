-- Approved rollout plans, their bounded steps, guardrail evidence and safety rollbacks.
CREATE TABLE rollout_plans (
    id text PRIMARY KEY,
    project_id text NOT NULL,
    environment_id text NOT NULL,
    flag_key text NOT NULL CHECK (length(flag_key) BETWEEN 1 AND 128),
    run_id text NOT NULL,
    mode text NOT NULL CHECK (mode IN ('scheduled','metric')),
    ceiling_bp integer NOT NULL CHECK (ceiling_bp BETWEEN 1 AND 10000),
    base_revision bigint NOT NULL CHECK (base_revision >= 1),
    expected_revision bigint CHECK (expected_revision >= base_revision),
    guardrails jsonb NOT NULL,
    plan_hash text NOT NULL CHECK (plan_hash ~ '^[0-9a-f]{64}$'),
    rationale text NOT NULL CHECK (length(rationale) BETWEEN 1 AND 512),
    proposer_id text NOT NULL REFERENCES users(id),
    state text NOT NULL CHECK (state IN ('proposed','approved','running','completed','cancelled','rolled_back','stale','rejected','expired')),
    approver_id text REFERENCES users(id),
    approved_at timestamptz,
    approval_expires_at timestamptz,
    started_at timestamptz,
    finished_at timestamptz,
    finish_reason text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    FOREIGN KEY (project_id, environment_id) REFERENCES environments(project_id, id),
    FOREIGN KEY (project_id, environment_id, run_id) REFERENCES experiment_runs(project_id, environment_id, id),
    CHECK (state NOT IN ('approved','running','completed','cancelled','rolled_back') OR approver_id IS NOT NULL),
    CHECK (approver_id IS NULL OR approver_id <> proposer_id),
    CHECK (state NOT IN ('running') OR (started_at IS NOT NULL AND expected_revision IS NOT NULL)),
    CHECK (state NOT IN ('completed','cancelled','rolled_back','stale','rejected','expired') OR finished_at IS NOT NULL)
);
CREATE INDEX rollout_plans_active_idx ON rollout_plans(state) WHERE state IN ('approved','running');
CREATE INDEX rollout_plans_project_idx ON rollout_plans(project_id, created_at DESC, id);
-- At most one live plan may drive a run's traffic.
CREATE UNIQUE INDEX rollout_plans_one_live_idx ON rollout_plans(project_id, environment_id, run_id) WHERE state IN ('proposed','approved','running');

CREATE TABLE rollout_steps (
    plan_id text NOT NULL REFERENCES rollout_plans(id),
    ordinal integer NOT NULL CHECK (ordinal >= 1),
    traffic_bp integer NOT NULL CHECK (traffic_bp BETWEEN 0 AND 10000),
    offset_seconds integer NOT NULL CHECK (offset_seconds BETWEEN 0 AND 604800),
    due_at timestamptz,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','applied','cancelled')),
    applied_revision bigint,
    applied_at timestamptz,
    PRIMARY KEY (plan_id, ordinal),
    CHECK (state <> 'applied' OR (applied_revision IS NOT NULL AND applied_at IS NOT NULL))
);

CREATE TABLE guardrail_checks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    plan_id text NOT NULL REFERENCES rollout_plans(id),
    checked_at timestamptz NOT NULL,
    decision text NOT NULL CHECK (decision IN ('pass','insufficient','breach')),
    evidence jsonb NOT NULL
);
CREATE INDEX guardrail_checks_plan_idx ON guardrail_checks(plan_id, id DESC);

CREATE TABLE safety_rollbacks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    plan_id text NOT NULL REFERENCES rollout_plans(id),
    project_id text NOT NULL,
    environment_id text NOT NULL,
    flag_key text NOT NULL,
    run_id text NOT NULL,
    revision_before bigint NOT NULL,
    revision_after bigint NOT NULL,
    rolled_back_at timestamptz NOT NULL,
    cooldown_until timestamptz NOT NULL,
    evidence jsonb NOT NULL,
    UNIQUE (plan_id)
);
CREATE INDEX safety_rollbacks_flag_idx ON safety_rollbacks(project_id, environment_id, flag_key, cooldown_until DESC);

CREATE FUNCTION rollout_plans_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE','TRUNCATE') THEN
        RAISE EXCEPTION 'rollout plans are retained';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.project_id IS DISTINCT FROM OLD.project_id
       OR NEW.environment_id IS DISTINCT FROM OLD.environment_id OR NEW.flag_key IS DISTINCT FROM OLD.flag_key
       OR NEW.run_id IS DISTINCT FROM OLD.run_id OR NEW.mode IS DISTINCT FROM OLD.mode
       OR NEW.ceiling_bp IS DISTINCT FROM OLD.ceiling_bp OR NEW.base_revision IS DISTINCT FROM OLD.base_revision
       OR NEW.guardrails IS DISTINCT FROM OLD.guardrails OR NEW.plan_hash IS DISTINCT FROM OLD.plan_hash
       OR NEW.rationale IS DISTINCT FROM OLD.rationale OR NEW.proposer_id IS DISTINCT FROM OLD.proposer_id
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'rollout plan content is immutable';
    END IF;
    IF OLD.state IN ('completed','cancelled','rolled_back','stale','rejected','expired') AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'rollout plan is final';
    END IF;
    IF NEW.state IS DISTINCT FROM OLD.state AND NOT (
        (OLD.state = 'proposed' AND NEW.state IN ('approved','rejected','stale')) OR
        (OLD.state = 'approved' AND NEW.state IN ('running','rejected','stale','expired')) OR
        (OLD.state = 'running' AND NEW.state IN ('completed','cancelled','rolled_back','stale'))
    ) THEN
        RAISE EXCEPTION 'invalid rollout plan transition % to %', OLD.state, NEW.state;
    END IF;
    IF OLD.approver_id IS NOT NULL AND (NEW.approver_id IS DISTINCT FROM OLD.approver_id
       OR NEW.approved_at IS DISTINCT FROM OLD.approved_at OR NEW.approval_expires_at IS DISTINCT FROM OLD.approval_expires_at) THEN
        RAISE EXCEPTION 'approval is immutable';
    END IF;
    IF OLD.expected_revision IS NOT NULL AND NEW.expected_revision < OLD.expected_revision THEN
        RAISE EXCEPTION 'expected revision cannot move backwards';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER rollout_plans_guard BEFORE UPDATE OR DELETE ON rollout_plans
    FOR EACH ROW EXECUTE FUNCTION rollout_plans_guard();
CREATE TRIGGER rollout_plans_no_truncate BEFORE TRUNCATE ON rollout_plans
    FOR EACH STATEMENT EXECUTE FUNCTION rollout_plans_guard();

CREATE FUNCTION rollout_steps_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE','TRUNCATE') THEN
        RAISE EXCEPTION 'rollout steps are retained';
    END IF;
    IF NEW.plan_id IS DISTINCT FROM OLD.plan_id OR NEW.ordinal IS DISTINCT FROM OLD.ordinal
       OR NEW.traffic_bp IS DISTINCT FROM OLD.traffic_bp OR NEW.offset_seconds IS DISTINCT FROM OLD.offset_seconds THEN
        RAISE EXCEPTION 'rollout step content is immutable';
    END IF;
    IF OLD.state <> 'pending' AND NEW IS DISTINCT FROM OLD THEN
        RAISE EXCEPTION 'rollout step is final';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER rollout_steps_guard BEFORE UPDATE OR DELETE ON rollout_steps
    FOR EACH ROW EXECUTE FUNCTION rollout_steps_guard();
CREATE TRIGGER rollout_steps_no_truncate BEFORE TRUNCATE ON rollout_steps
    FOR EACH STATEMENT EXECUTE FUNCTION rollout_steps_guard();

CREATE FUNCTION evidence_no_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'guardrail and rollback evidence is append-only'; END $$;
CREATE TRIGGER guardrail_checks_no_rewrite BEFORE UPDATE OR DELETE ON guardrail_checks
    FOR EACH ROW EXECUTE FUNCTION evidence_no_rewrite();
CREATE TRIGGER safety_rollbacks_no_rewrite BEFORE UPDATE OR DELETE ON safety_rollbacks
    FOR EACH ROW EXECUTE FUNCTION evidence_no_rewrite();

-- Attribution identity for automated progression and safety rollback. It is inactive, has an
-- unusable password hash and no project membership, so it can never authenticate or authorize.
INSERT INTO users(id,email,password_hash,role,active)
VALUES ('system:rollout','system-rollout@switchyard.invalid','!','developer',false)
ON CONFLICT (id) DO NOTHING;
