package janitor

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/store"
)

var now = time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)

const (
	old    = "2026-04-01T00:00:00Z" // more than 6 months before now
	recent = "2026-09-01T00:00:00Z"
)

// setup seeds two users with identical data: an old and a recent item, each
// with an action, outbox entry and run, plus an old digest and two sessions.
func setup(t *testing.T) (*Janitor, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.OpenFile(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db := st.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n'), ('u2','u2','x','n')`)
	exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('s1','u1','miniflux_feed','1','A'), ('s2','u2','miniflux_feed','1','B')`)
	exec(`INSERT INTO connections (id, user_id, kind, base_url, secret_enc) VALUES ('c1','u1','miniflux','https://example.com','x')`)
	for _, u := range []struct{ user, src string }{{"u1", "s1"}, {"u2", "s2"}} {
		p := u.user + "-"
		exec(`INSERT INTO digests (id, user_id, source_id, batch_date, state) VALUES (?, ?, ?, '2026-04-01', 'light')`, p+"digold", u.user, u.src)
		for _, it := range []struct{ id, created, digest string }{{"old", old, p + "digold"}, {"new", recent, ""}} {
			var d any
			if it.digest != "" {
				d = it.digest
			}
			exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, digest_id, created_at, updated_at)
				VALUES (?, ?, ?, ?, 'article', 'https://example.com/x', 't', 'pending_light', ?, ?, ?)`,
				p+it.id, u.user, u.src, p+it.id, d, it.created, it.created)
			exec(`INSERT INTO actions (action_id, user_id, subject_type, subject_id, kind, client_at, received_at, release_at, status)
				VALUES (?, ?, 'item', ?, 'archive', ?, ?, ?, 'released')`, p+"act"+it.id, u.user, p+it.id, it.created, it.created, it.created)
			exec(`INSERT INTO outbox (id, user_id, action_id, op, created_at) VALUES (?, ?, ?, 'x', ?)`, p+"ob"+it.id, u.user, p+"act"+it.id, it.created)
			exec(`INSERT INTO runs (id, user_id, started_at) VALUES (?, ?, ?)`, p+"run"+it.id, u.user, it.created)
			exec(`INSERT INTO run_items (run_id, user_id, subject_id) VALUES (?, ?, ?)`, p+"run"+it.id, u.user, p+it.id)
		}
		exec(`INSERT INTO sessions (id, user_id, created_at, expires_at, last_seen_at) VALUES
			(?, ?, ?, '2026-01-01T00:00:00Z', ?), (?, ?, ?, '2027-01-01T00:00:00Z', ?)`,
			p+"s-exp", u.user, old, old, p+"s-live", u.user, recent, recent)
	}
	return &Janitor{DB: db, BackupDir: filepath.Join(dir, "backups"), Now: func() time.Time { return now }}, db
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPruneDeletesOnlyOldRowsOfThatUser(t *testing.T) {
	j, db := setup(t)
	res, err := j.Prune(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Items != 1 || res.Digests != 1 || res.Actions != 1 || res.Runs != 1 {
		t.Errorf("result = %+v", res)
	}
	// want[table] = {u1 rows left, u2 rows left}. Digests hold only the old one.
	want := map[string][2]int{
		"items": {1, 2}, "digests": {0, 1}, "actions": {1, 2},
		"outbox": {1, 2}, "runs": {1, 2}, "run_items": {1, 2}, "sessions": {1, 2},
		"sources": {1, 1}, "connections": {1, 0}, // settings are never pruned
	}
	for tbl, w := range want {
		for i, u := range []string{"u1", "u2"} {
			if n := count(t, db, `SELECT COUNT(*) FROM `+tbl+` WHERE user_id = ?`, u); n != w[i] {
				t.Errorf("%s rows for %s = %d, want %d", tbl, u, n, w[i])
			}
		}
	}
}

func TestPruneIsIdempotent(t *testing.T) {
	j, db := setup(t)
	ctx := context.Background()
	if _, err := j.Prune(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	before := count(t, db, `SELECT COUNT(*) FROM items`)
	res, err := j.Prune(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{}) {
		t.Errorf("second prune deleted %+v", res)
	}
	if after := count(t, db, `SELECT COUNT(*) FROM items`); after != before {
		t.Errorf("items %d -> %d", before, after)
	}
}

func TestPruneDeletesUntriagedItems(t *testing.T) {
	j, db := setup(t)
	if _, err := db.Exec(`UPDATE items SET state = 'light' WHERE id = 'u1-old'`); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Prune(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM items WHERE id = 'u1-old'`); n != 0 {
		t.Error("untriaged old item survived")
	}
}

func TestPruneKeepsDigestWithRecentChild(t *testing.T) {
	j, db := setup(t)
	if _, err := db.Exec(`UPDATE items SET digest_id = 'u1-digold' WHERE id = 'u1-new'`); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Prune(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM digests WHERE id = 'u1-digold'`); n != 1 {
		t.Error("digest with a surviving child was deleted")
	}
}

func TestPruneNeedsUser(t *testing.T) {
	j, _ := setup(t)
	if _, err := j.Prune(context.Background(), ""); err == nil {
		t.Error("expected an error for an empty user ID")
	}
}

func TestSnapshotWritesOnePerDayAndKeepsSeven(t *testing.T) {
	j, db := setup(t)
	ctx := context.Background()
	wrote, err := j.Snapshot(ctx)
	if err != nil || !wrote {
		t.Fatalf("first snapshot: wrote=%v err=%v", wrote, err)
	}
	wrote, err = j.Snapshot(ctx)
	if err != nil || wrote {
		t.Fatalf("same-day snapshot: wrote=%v err=%v", wrote, err)
	}

	// The snapshot is a usable database holding the same rows.
	snap, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(j.BackupDir, "siftstr-2026-10-09.db")))
	if err != nil {
		t.Fatal(err)
	}
	got := count(t, snap, `SELECT COUNT(*) FROM items`)
	snap.Close() // Windows cannot delete an open file
	if want := count(t, db, `SELECT COUNT(*) FROM items`); got != want {
		t.Errorf("snapshot items = %d, want %d", got, want)
	}

	for d := 1; d <= 9; d++ {
		day := now.AddDate(0, 0, d)
		j.Now = func() time.Time { return day }
		if _, err := j.Snapshot(ctx); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(j.BackupDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != KeepSnapshots {
		t.Fatalf("%d files kept, want %d", len(entries), KeepSnapshots)
	}
	if first := entries[0].Name(); first != "siftstr-2026-10-12.db" {
		t.Errorf("oldest kept = %s", first)
	}
}
