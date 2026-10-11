-- Where an item came from, when that is more specific than its source: a
-- Karakeep bookmark names the site or publisher it was saved from. Cards and
-- the briefing show it in place of the source name; empty means use the
-- source's name.
ALTER TABLE items ADD COLUMN byline TEXT;
