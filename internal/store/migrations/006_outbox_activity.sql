-- The Activity page tells a stopped entry that ran out of retries ("paused")
-- from one a retry cannot fix ("failed"), and lets the user dismiss one.
ALTER TABLE outbox ADD COLUMN permanent INTEGER NOT NULL DEFAULT 0;
ALTER TABLE outbox ADD COLUMN dismissed_at TEXT;
