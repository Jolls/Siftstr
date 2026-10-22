package writeback

import "testing"

// Once the bookmark is deleted, the item has none: its title must not link to
// a Karakeep page that is gone (the card stays on the page with its badge).
func TestDeletingTheBookmarkClearsTheItemsBookmarkID(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.karakeepItem("i1", "srck", "light")
	f.karakeepItem("i2", "srckk", "light") // not deleted: keeps its bookmark
	f.act("archive", "i1")
	f.release()
	f.drain()
	if got := f.bookmarkID("i1"); got != "" {
		t.Errorf("deleted item still has bookmark %q", got)
	}
	if got := f.bookmarkID("i2"); got != "bm_i2" {
		t.Errorf("another item's bookmark = %q, want it left alone", got)
	}
}

func TestKeepWithoutKarakeepAsDestinationClearsTheDeletedBookmarkID(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep, MeTube)
	f.exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'video_destinations', 'metube')`)
	f.item("i1", "srck", "video", "light", "41")
	f.exec(`UPDATE items SET karakeep_id = 'bm_i1' WHERE id = 'i1'`)
	f.act("keep", "i1")
	f.release()
	f.drain()
	if got := f.bookmarkID("i1"); got != "" {
		t.Errorf("the bookmark was deleted but the item still has %q", got)
	}
}
