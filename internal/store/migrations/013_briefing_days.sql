-- The days that have a briefing file (ARCHITECTURE.md section 8). The timer
-- writes a day's file whether or not Claude ran, so /briefings and the closing
-- of earlier days can't rely on a summarized run. Deleted with the rest of the
-- workflow state.
CREATE TABLE briefing_days (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day        TEXT NOT NULL,
    written_at TEXT NOT NULL,
    PRIMARY KEY (user_id, day)
);
