package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

// Rebuilding sources for migration 7 must not cascade into the rows that
// reference it, and the new column and kind must work afterwards.
func TestSourcesRebuildKeepsItems(t *testing.T) {
	ctx := context.Background()
	old := fstest.MapFS{}
	entries, _ := fs.ReadDir(migrationsFS, "migrations")
	for _, e := range entries {
		if e.Name() >= "007" {
			continue
		}
		b, _ := fs.ReadFile(migrationsFS, "migrations/"+e.Name())
		old["migrations/"+e.Name()] = &fstest.MapFile{Data: b}
	}
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	defer s.Close()
	if err := s.migrate(ctx, old, "migrations"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1', 'u1', 'x', 'now')`,
		`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('s1', 'u1', 'miniflux_feed', '1', 'Feed')`,
		`INSERT INTO sources (id, user_id, kind, external_id, name, granularity) VALUES ('s2', 'u1', 'miniflux_feed', '2', 'Digest', 'digest')`,
		`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		   VALUES ('i1', 'u1', 's1', 'e1', 'article', 'https://example.com', 't', 'pending_light', '2026-10-01T08:00:00Z', 'now')`,
		`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		   VALUES ('i2', 'u1', 's2', 'e2', 'article', 'https://example.com/2', 't', 'pending_light', '2026-10-01T08:00:00Z', 'now')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx, migrationsFS, "migrations"); err != nil {
		t.Fatal(err)
	}

	var state, day string
	if err := db.QueryRowContext(ctx, `SELECT state, COALESCE(batch_date, '') FROM items WHERE id = 'i1'`).Scan(&state, &day); err != nil {
		t.Fatalf("item lost in the rebuild: %v", err)
	}
	if state != "light" || day != "2026-09-30" {
		t.Errorf("individual item = %s on %q, want light on 2026-09-30, the Backlog", state, day)
	}
	if err := db.QueryRowContext(ctx, `SELECT state FROM items WHERE id = 'i2'`).Scan(&state); err != nil || state != "pending_light" {
		t.Errorf("digest entry = %q, %v, want it still waiting for its digest", state, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sources (id, user_id, kind, external_id, name, summary_source) VALUES ('s3', 'u1', 'karakeep', 'karakeep', 'Karakeep', 'karakeep')`); err != nil {
		t.Errorf("karakeep source: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		VALUES ('i3', 'u1', 'nope', 'e3', 'article', 'u', 't', 'light', 'n', 'n')`); err == nil {
		t.Error("foreign keys are not enforced after the migration")
	}
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenAppliesMigrationsOnceAndUsesWAL(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := OpenFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	_ = s.Close()

	// Reopening must not re-apply migrations.
	s, err = OpenFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != 13 {
		t.Fatalf("migrations = %d, %v", n, err)
	}
}

// Every user-owned table must carry user_id from migration 1.
func TestUserOwnedTablesHaveUserID(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, table := range []string{"sessions", "api_keys", "connections", "sources", "digests", "items", "actions", "outbox", "runs", "user_settings", "old_bookmark_cache", "briefing_days"} {
		rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var name string
			_ = rows.Scan(&name)
			if name == "user_id" {
				found = true
			}
		}
		_ = rows.Close()
		if !found {
			t.Errorf("%s has no user_id", table)
		}
	}
}

// An item cannot point at another user's source.
func TestItemCannotReferenceOtherUsersSource(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	exec := func(q string, args ...any) error {
		_, err := s.db.ExecContext(ctx, q, args...)
		return err
	}
	for _, u := range []string{"u1", "u2"} {
		if err := exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, 'x', 'now')`, u, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('s1', 'u1', 'miniflux_feed', '1', 'Feed')`); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		VALUES (?, ?, 's1', ?, 'article', 'https://example.com', 't', 'pending_light', 'now', 'now')`
	if err := exec(insert, "i1", "u1", "e1"); err != nil {
		t.Fatalf("own source: %v", err)
	}
	if err := exec(insert, "i2", "u2", "e2"); err == nil {
		t.Fatal("item for u2 referencing u1's source was accepted")
	}
}

// Transactions take the write lock when they begin, so a transaction that
// reads before it writes cannot hit BUSY on lock upgrade.
func TestTransactionsTakeTheWriteLockUpFront(t *testing.T) {
	ctx := context.Background()
	s, err := OpenFile(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	// A second writer has to wait, even though tx has not written anything.
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	other, err := s.db.BeginTx(short, nil)
	if err == nil {
		other.Rollback()
		t.Fatal("second transaction began while the first held the write lock")
	}
}
