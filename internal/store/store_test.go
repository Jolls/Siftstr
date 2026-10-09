package store

import (
	"context"
	"path/filepath"
	"testing"
)

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
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("migrations = %d, %v", n, err)
	}
}

// Every user-owned table must carry user_id from migration 1.
func TestUserOwnedTablesHaveUserID(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, table := range []string{"sessions", "api_keys", "connections", "sources", "digests", "items", "actions", "outbox", "runs", "user_settings"} {
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
