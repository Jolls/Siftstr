-- siftstr:foreign-keys-off
-- Karakeep as a source, and a per-source summary_source (ARCHITECTURE.md
-- section 6). SQLite cannot change a CHECK constraint in place, so sources is
-- rebuilt. The runner turns foreign keys off for this file, so dropping the
-- old table does not cascade into items and digests, and checks them again
-- before committing.

CREATE TABLE sources_new (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('miniflux_feed', 'nostr', 'karakeep')),
    external_id     TEXT NOT NULL,
    name            TEXT NOT NULL,
    category        TEXT NOT NULL DEFAULT '',
    granularity     TEXT NOT NULL DEFAULT 'individual' CHECK (granularity IN ('individual', 'digest')),
    max_depth       TEXT NOT NULL DEFAULT 'deep' CHECK (max_depth IN ('light', 'deep')),
    carryover       TEXT NOT NULL DEFAULT 'carry' CHECK (carryover IN ('carry', 'drop')),
    prompt_override TEXT,
    enabled         INTEGER NOT NULL DEFAULT 1,
    summary_source  TEXT NOT NULL DEFAULT 'agent' CHECK (summary_source IN ('agent', 'karakeep')),
    UNIQUE (user_id, kind, external_id),
    UNIQUE (user_id, id)
);
INSERT INTO sources_new (id, user_id, kind, external_id, name, category, granularity, max_depth, carryover, prompt_override, enabled)
    SELECT id, user_id, kind, external_id, name, category, granularity, max_depth, carryover, prompt_override, enabled FROM sources;
DROP TABLE sources;
ALTER TABLE sources_new RENAME TO sources;

-- The Karakeep bookmark of an item, once it has one: set when Siftstr creates
-- the bookmark at ingest or at promote, or at ingest for a bookmark that
-- Karakeep collected itself.
ALTER TABLE items ADD COLUMN karakeep_id TEXT;

-- Items are triaged on their title and excerpt as soon as they are ingested,
-- so an individual item no longer waits for a Claude summary. Items already
-- waiting for one under the old flow become ready to triage, in the Backlog:
-- the day before their UTC ingest date can never be later than the user's
-- local today, so they show up whatever the timezone. Digest entries keep
-- waiting for their digest's summary.
UPDATE items SET state = 'light', batch_date = date(substr(created_at, 1, 10), '-1 day')
    WHERE state = 'pending_light' AND digest_id IS NULL
      AND source_id IN (SELECT id FROM sources WHERE granularity = 'individual');
