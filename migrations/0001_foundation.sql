CREATE TABLE service_metadata (
    key text PRIMARY KEY,
    value text NOT NULL
);
INSERT INTO service_metadata(key, value) VALUES ('service', 'switchyard');
