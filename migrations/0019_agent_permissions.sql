-- An agent identity may read one environment's configuration and submit a proposal.
-- It cannot also evaluate or ingest, and it has no approve or apply permission.
ALTER TABLE application_keys DROP CONSTRAINT application_keys_permissions_check;
ALTER TABLE application_keys ADD CONSTRAINT application_keys_permissions_check CHECK (
    cardinality(permissions) BETWEEN 1 AND 3
    AND (
        permissions <@ ARRAY['evaluate', 'events:write', 'config:read']::text[]
        OR permissions <@ ARRAY['context:read', 'proposals:submit']::text[]
    )
);
