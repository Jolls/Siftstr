-- Old unarchived Karakeep bookmarks (issue #46, ARCHITECTURE.md sections 6 and
-- 8). The old-sifting job walks the unarchived list a few pages per run and
-- caches the ID and creation time of each bookmark older than the age limit
-- that is not yet in Siftstr; the daily pull brings the oldest into the
-- Backlog. Karakeep stays the system of record: the cache can always be
-- rebuilt, and the janitor never deletes from it.
CREATE TABLE old_bookmark_cache (
    user_id             TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    bookmark_id         TEXT NOT NULL,
    bookmark_created_at TEXT NOT NULL,   -- the bookmark's createdAt in Karakeep, RFC 3339 UTC
    pulled_on           TEXT,            -- user-local day it was brought into the Backlog
    PRIMARY KEY (user_id, bookmark_id)
);

CREATE INDEX old_bookmark_cache_next ON old_bookmark_cache (user_id, pulled_on, bookmark_created_at, bookmark_id);
