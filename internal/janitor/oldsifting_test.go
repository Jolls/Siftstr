package janitor

import (
	"context"
	"testing"
)

// The old-bookmark cache is working data (Karakeep stays the system of record),
// so the six-month cleanup never touches it, however old a row is.
func TestPruneNeverTouchesTheOldBookmarkCache(t *testing.T) {
	j, db := setup(t)
	for _, u := range []string{"u1", "u2"} {
		if _, err := db.Exec(`INSERT INTO old_bookmark_cache (user_id, bookmark_id, bookmark_created_at, pulled_on) VALUES (?, 'b1', '2020-01-01T00:00:00Z', '2026-01-01')`, u); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.Prune(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"u1", "u2"} {
		if n := count(t, db, `SELECT COUNT(*) FROM old_bookmark_cache WHERE user_id = ?`, u); n != 1 {
			t.Errorf("%s cache rows = %d, want 1", u, n)
		}
	}
}

func TestPruneNeverTouchesUserSettings(t *testing.T) {
	j, db := setup(t)
	for k, v := range map[string]string{"old_sifting_cursor": "40", "karakeep_walked_back_to": "x"} {
		if _, err := db.Exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', ?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := j.Prune(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM user_settings WHERE user_id = 'u1'`); n != 2 {
		t.Errorf("settings rows = %d, want 2", n)
	}
}
