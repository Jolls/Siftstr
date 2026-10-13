-- The last connection check per connection (issue #13), shown on Settings →
-- Destinations. Written when a connection is saved or tested, and kept current
-- by ingest and the outbox. NULL check_state means never checked. No CHECK
-- constraint: SQLite cannot change one in place, and Go validates the values.
ALTER TABLE connections ADD COLUMN check_state  TEXT;                       -- ok | unreachable | tls | auth | unexpected
ALTER TABLE connections ADD COLUMN check_detail TEXT NOT NULL DEFAULT '';   -- short reason, never a secret
ALTER TABLE connections ADD COLUMN check_via    TEXT NOT NULL DEFAULT '';   -- save | test | ingest | outbox
ALTER TABLE connections ADD COLUMN checked_at   TEXT;                       -- RFC 3339 UTC
