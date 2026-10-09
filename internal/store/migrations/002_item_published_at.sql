-- When the source published the item (RFC 3339 UTC). Null when the upstream
-- gave no date. created_at stays the time Siftstr first saw it.
ALTER TABLE items ADD COLUMN published_at TEXT;
