ALTER TABLE work_dead_letters ADD COLUMN resolved_at timestamptz;
CREATE INDEX work_dead_letters_open_idx ON work_dead_letters(created_at,stream_name,stream_sequence) WHERE resolved_at IS NULL;
CREATE INDEX raw_events_replay_idx ON raw_events(project_id,environment_id,run_id,event_id,received_at);
