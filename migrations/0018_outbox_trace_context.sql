-- W3C trace context captured when durable work is recorded, so a trace can follow an accepted
-- event through publication and processing. Empty when the originating request was unsampled.
ALTER TABLE outbox ADD COLUMN traceparent text NOT NULL DEFAULT ''
    CHECK (traceparent = '' OR traceparent ~ '^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$');
