package janitor

import (
	"context"
	"testing"
)

func TestPruneDeletesOldHighlightPicksOfThatUserOnly(t *testing.T) {
	j, db := setup(t)
	for _, u := range []string{"u1", "u2"} {
		for _, day := range []string{"2026-04-01", "2026-09-01"} {
			if _, err := db.Exec(`INSERT INTO highlight_picks (user_id, day, highlight_id, text, created_at) VALUES (?, ?, 'h', 't', 'c')`, u, day); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO highlight_cache (user_id, highlight_id, bookmark_id) VALUES (?, 'h', 'b')`, u); err != nil {
			t.Fatal(err)
		}
	}
	res, err := j.Prune(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Highlights != 1 {
		t.Errorf("deleted %d picks, want 1", res.Highlights)
	}
	for u, want := range map[string]int{"u1": 1, "u2": 2} {
		if n := count(t, db, `SELECT COUNT(*) FROM highlight_picks WHERE user_id = ?`, u); n != want {
			t.Errorf("%s has %d picks, want %d", u, n, want)
		}
		// The cache is working data for the next pick, not workflow history.
		if n := count(t, db, `SELECT COUNT(*) FROM highlight_cache WHERE user_id = ?`, u); n != 1 {
			t.Errorf("%s cache rows = %d, want 1", u, n)
		}
	}
}
