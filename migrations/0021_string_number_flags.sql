-- String and number flags use the same revision path as boolean and JSON.
-- Strings are JSON strings of at most 1024 bytes. Numbers are bounded JSON numbers.
ALTER TABLE flags DROP CONSTRAINT flags_type_check;
ALTER TABLE flags ADD CONSTRAINT flags_type_check CHECK (type IN ('boolean','json','string','number'));
ALTER TABLE proposals DROP CONSTRAINT proposals_flag_type_check;
ALTER TABLE proposals ADD CONSTRAINT proposals_flag_type_check CHECK (flag_type IN ('boolean','json','string','number'));
