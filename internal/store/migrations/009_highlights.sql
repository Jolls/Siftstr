-- Karakeep highlights in the daily briefing (ARCHITECTURE.md sections 8 and
-- 10). Karakeep has no count or random pick for highlights, so a weekly walk
-- caches their IDs here and the day's pick is chosen from the cache.

-- Every highlight with text, as of the last walk. shown_on is the user's local
-- day it was last picked, so it is not picked again until all have been shown.
CREATE TABLE highlight_cache (
    user_id      TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    highlight_id TEXT NOT NULL,
    bookmark_id  TEXT NOT NULL,
    shown_on     TEXT,
    PRIMARY KEY (user_id, highlight_id)
);

-- The highlight picked for one day. The text and the bookmark's name are
-- copied, because Karakeep can change or delete them later and a rewritten
-- briefing must read the same. Deleted with the rest of the workflow state.
CREATE TABLE highlight_picks (
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day            TEXT NOT NULL,
    highlight_id   TEXT NOT NULL,
    text           TEXT NOT NULL,
    note           TEXT NOT NULL DEFAULT '',
    color          TEXT NOT NULL DEFAULT '',
    bookmark_title TEXT NOT NULL DEFAULT '',
    bookmark_url   TEXT NOT NULL DEFAULT '',
    byline         TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    PRIMARY KEY (user_id, day)
);
