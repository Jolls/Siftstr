-- A link to the highlight's bookmark in Karakeep itself, next to the link to
-- the original article. Picks made before this column stay without one.
ALTER TABLE highlight_picks ADD COLUMN karakeep_url TEXT NOT NULL DEFAULT '';
