CREATE TABLE experiment_runs (
    id text PRIMARY KEY,
    project_id text NOT NULL,
    environment_id text NOT NULL,
    flag_id text NOT NULL,
    name text NOT NULL CHECK(length(name) BETWEEN 1 AND 128),
    state text NOT NULL CHECK(state IN ('draft','running','paused','completed')),
    control_variant_id text NOT NULL,
    definition jsonb NOT NULL,
    created_by text NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    completed_at timestamptz,
    UNIQUE(project_id,environment_id,id),
    FOREIGN KEY(project_id,flag_id) REFERENCES flags(project_id,id),
    FOREIGN KEY(project_id,environment_id) REFERENCES environments(project_id,id)
);
-- Draft and paused runs reserve the flag too: resuming must preserve its population.
CREATE UNIQUE INDEX experiment_reserved_flag_idx ON experiment_runs(flag_id,environment_id)
WHERE state <> 'completed';
CREATE INDEX experiment_scope_idx ON experiment_runs(project_id,environment_id,created_at DESC);
CREATE FUNCTION prevent_experiment_definition_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(NEW.id,NEW.project_id,NEW.environment_id,NEW.flag_id,NEW.name,NEW.control_variant_id,NEW.definition,NEW.created_by,NEW.created_at)
       IS DISTINCT FROM ROW(OLD.id,OLD.project_id,OLD.environment_id,OLD.flag_id,OLD.name,OLD.control_variant_id,OLD.definition,OLD.created_by,OLD.created_at) THEN
        RAISE EXCEPTION 'experiment population and treatments are immutable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER experiment_definition_no_rewrite BEFORE UPDATE ON experiment_runs
FOR EACH ROW EXECUTE FUNCTION prevent_experiment_definition_rewrite();
