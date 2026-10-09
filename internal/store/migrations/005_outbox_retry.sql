-- The outbox retries with backoff, and an effect is enqueued at most once per
-- dedupe key, so releasing the same action twice cannot send anything twice.
ALTER TABLE outbox ADD COLUMN next_attempt_at TEXT NOT NULL DEFAULT '';
ALTER TABLE outbox ADD COLUMN dedupe_key TEXT;
CREATE UNIQUE INDEX outbox_dedupe ON outbox(user_id, dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX outbox_due ON outbox(user_id, status, next_attempt_at);
