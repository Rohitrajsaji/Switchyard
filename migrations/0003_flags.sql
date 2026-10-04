CREATE TABLE flags (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects(id),
    key text NOT NULL CHECK (key ~ '^[a-z][a-z0-9_-]{0,63}$'),
    type text NOT NULL CHECK(type IN ('boolean','json')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE(project_id,key),
    UNIQUE(project_id,id)
);
CREATE TABLE flag_revisions (
    project_id text NOT NULL,
    environment_id text NOT NULL,
    flag_id text NOT NULL,
    revision bigint NOT NULL CHECK (revision>0),
    definition jsonb NOT NULL,
    created_by text NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(flag_id,environment_id,revision),
    FOREIGN KEY(project_id,flag_id) REFERENCES flags(project_id,id),
    FOREIGN KEY(project_id,environment_id) REFERENCES environments(project_id,id)
);
CREATE TABLE environment_flag_state (
    project_id text NOT NULL,
    environment_id text NOT NULL,
    flag_id text NOT NULL,
    current_revision bigint NOT NULL,
    PRIMARY KEY(flag_id,environment_id),
    FOREIGN KEY(project_id,flag_id) REFERENCES flags(project_id,id),
    FOREIGN KEY(project_id,environment_id) REFERENCES environments(project_id,id),
    FOREIGN KEY(flag_id,environment_id,current_revision) REFERENCES flag_revisions(flag_id,environment_id,revision)
);
CREATE INDEX flag_state_environment_idx ON environment_flag_state(project_id,environment_id);
CREATE FUNCTION prevent_flag_revision_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'flag revisions are immutable'; END;
$$;
CREATE TRIGGER flag_revision_no_rewrite BEFORE UPDATE OR DELETE ON flag_revisions
FOR EACH ROW EXECUTE FUNCTION prevent_flag_revision_rewrite();
