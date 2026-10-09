-- An action remembers the state it replaced, so undo and last-write-wins can
-- put the subject back, and why it was rejected, so a replay answers the same.
ALTER TABLE actions ADD COLUMN prev_state TEXT;
ALTER TABLE actions ADD COLUMN reason TEXT NOT NULL DEFAULT '';

-- The hold queries only ever look at held actions, which are few.
CREATE INDEX actions_held_subject ON actions(user_id, subject_type, subject_id, client_at) WHERE status = 'held';
CREATE INDEX actions_held_release ON actions(user_id, release_at) WHERE status = 'held';
