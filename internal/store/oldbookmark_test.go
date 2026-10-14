package store

import (
	"context"
	"testing"
)

func TestOldBookmarkCacheCascadesWithUser(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n')`,
		`INSERT INTO old_bookmark_cache (user_id, bookmark_id, bookmark_created_at) VALUES ('u1', 'b1', '2020-01-01T00:00:00Z')`,
		`DELETE FROM users WHERE id = 'u1'`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM old_bookmark_cache`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows left = %d, %v", n, err)
	}
}
