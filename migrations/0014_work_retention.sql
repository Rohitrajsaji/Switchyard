-- Cleanup only completed history; pending/unresolved work remains inspectable.
CREATE INDEX outbox_retention_idx ON outbox(published_at,id) WHERE published_at IS NOT NULL;
CREATE INDEX work_dead_letter_retention_idx ON work_dead_letters(resolved_at,stream_name,stream_sequence) WHERE resolved_at IS NOT NULL;
CREATE INDEX work_dead_letter_message_idx ON work_dead_letters(message_id) WHERE resolved_at IS NULL;
