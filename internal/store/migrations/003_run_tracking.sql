-- A run remembers its local day, and what it offered to Claude, so that a
-- summary is accepted only for something that run actually sent and lands on
-- the day the run belonged to.
ALTER TABLE runs ADD COLUMN day TEXT NOT NULL DEFAULT '';

CREATE TABLE run_items (
    run_id     TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subject_id TEXT NOT NULL, -- an item ID, or a digest ID
    PRIMARY KEY (run_id, subject_id)
);
CREATE INDEX run_items_user ON run_items(user_id);
