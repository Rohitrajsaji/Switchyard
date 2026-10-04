CREATE TABLE users (
    id text PRIMARY KEY,
    email text NOT NULL UNIQUE CHECK (email = lower(email) AND length(email) BETWEEN 3 AND 254),
    password_hash text NOT NULL,
    role text NOT NULL CHECK (role IN ('viewer','developer','admin')),
    active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE sessions (
    token_hash bytea PRIMARY KEY,
    user_id text NOT NULL REFERENCES users(id),
    csrf_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_expiry_idx ON sessions(expires_at);
CREATE TABLE projects (
    id text PRIMARY KEY,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE project_memberships (
    project_id text NOT NULL REFERENCES projects(id),
    user_id text NOT NULL REFERENCES users(id),
    PRIMARY KEY(project_id,user_id)
);
CREATE INDEX memberships_user_idx ON project_memberships(user_id,project_id);
CREATE TABLE environments (
    id text PRIMARY KEY,
    project_id text NOT NULL REFERENCES projects(id),
    name text NOT NULL CHECK (name IN ('development','staging','production')),
    UNIQUE(project_id,name),
    UNIQUE(project_id,id)
);
CREATE TABLE application_keys (
    id text PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE,
    project_id text NOT NULL,
    environment_id text NOT NULL,
    name text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    permissions text[] NOT NULL CHECK (cardinality(permissions)>0 AND permissions <@ ARRAY['evaluate','events:write','config:read']),
    created_by text NOT NULL REFERENCES users(id),
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY(project_id,environment_id) REFERENCES environments(project_id,id)
);
CREATE TABLE audit_entries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_id text NOT NULL,
    source text NOT NULL CHECK (source IN ('human','application','system','agent')),
    project_id text REFERENCES projects(id),
    environment_id text,
    action text NOT NULL,
    request_id text NOT NULL,
    reason text NOT NULL,
    before_revision bigint,
    after_revision bigint,
    details jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY(project_id,environment_id) REFERENCES environments(project_id,id)
);
CREATE INDEX audit_project_time_idx ON audit_entries(project_id,created_at DESC,id DESC);
CREATE FUNCTION prevent_audit_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'audit history is append-only'; END;
$$;
CREATE TRIGGER audit_no_rewrite BEFORE UPDATE OR DELETE ON audit_entries
FOR EACH ROW EXECUTE FUNCTION prevent_audit_rewrite();
CREATE TRIGGER audit_no_truncate BEFORE TRUNCATE ON audit_entries
FOR EACH STATEMENT EXECUTE FUNCTION prevent_audit_rewrite();
