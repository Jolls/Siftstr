package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestAPIKeyLifecycle(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)

	k, secret, err := s.CreateAPIKey(ctx, u.ID, "  claude task ")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, KeyPrefix) || k.Label != "claude task" {
		t.Fatalf("key = %+v secret = %q", k, secret)
	}

	// Only a hash is stored.
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys WHERE key_hash = ?`, secret).Scan(&n)
	if n != 0 {
		t.Fatal("raw key stored in database")
	}

	*clock = clock.Add(time.Minute)
	got, err := s.UserForAPIKey(ctx, secret)
	if err != nil || got.ID != u.ID {
		t.Fatalf("lookup: %v", err)
	}
	keys, _ := s.ListAPIKeys(ctx, u.ID)
	if len(keys) != 1 || keys[0].LastUsedAt == nil || keys[0].RevokedAt != nil {
		t.Fatalf("list = %+v", keys)
	}

	if err := s.RevokeAPIKey(ctx, u.ID, k.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeAPIKey(ctx, u.ID, k.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if _, err := s.UserForAPIKey(ctx, secret); !errors.Is(err, ErrNoSession) {
		t.Fatalf("revoked key still works: %v", err)
	}
}

func TestAPIKeyRejectsBadKeys(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	_, secret, _ := s.CreateAPIKey(ctx, u.ID, "")
	for _, bad := range []string{"", "nope", KeyPrefix + "forged", strings.TrimPrefix(secret, KeyPrefix)} {
		if _, err := s.UserForAPIKey(ctx, bad); !errors.Is(err, ErrNoSession) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	// A session token is not an API key, and vice versa.
	token, _, _ := s.NewSession(ctx, u.ID)
	if _, err := s.UserForAPIKey(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Error("session token accepted as API key")
	}
	if _, err := s.UserForSession(ctx, secret); !errors.Is(err, ErrNoSession) {
		t.Error("API key accepted as session token")
	}
}

func TestDisabledUsersKeyStopsWorking(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	_, secret, _ := s.CreateAPIKey(ctx, u.ID, "")
	_, _ = s.db.ExecContext(ctx, `UPDATE users SET disabled = 1 WHERE id = ?`, u.ID)
	if _, err := s.UserForAPIKey(ctx, secret); !errors.Is(err, ErrNoSession) {
		t.Fatalf("disabled user's key works: %v", err)
	}
}

// Isolation: one user can neither list nor revoke another's keys, and each
// key resolves to its own owner.
func TestAPIKeysAreIsolatedBetweenUsers(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	alice, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	bob, _ := s.CreateUser(ctx, "bob", "password-two", RoleUser)
	ak, aSecret, _ := s.CreateAPIKey(ctx, alice.ID, "a")
	_, bSecret, _ := s.CreateAPIKey(ctx, bob.ID, "b")

	if keys, _ := s.ListAPIKeys(ctx, bob.ID); len(keys) != 1 || keys[0].ID == ak.ID {
		t.Fatalf("bob sees %+v", keys)
	}
	if err := s.RevokeAPIKey(ctx, bob.ID, ak.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob revoked alice's key: %v", err)
	}
	if _, err := s.UserForAPIKey(ctx, aSecret); err != nil {
		t.Fatalf("alice's key broken by bob's attempt: %v", err)
	}
	if got, _ := s.UserForAPIKey(ctx, aSecret); got.ID != alice.ID {
		t.Error("alice's key resolved to the wrong user")
	}
	if got, _ := s.UserForAPIKey(ctx, bSecret); got.ID != bob.ID {
		t.Error("bob's key resolved to the wrong user")
	}
	if err := s.RevokeAPIKey(ctx, alice.ID, "key_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown key: %v", err)
	}
}

func TestUserByUsername(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	u, _ := s.CreateUser(ctx, "Alice", "password-one", RoleUser)
	got, err := s.UserByUsername(ctx, " ALICE")
	if err != nil || got.ID != u.ID {
		t.Fatalf("%v", err)
	}
	if _, err := s.UserByUsername(ctx, "ghost"); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("unknown: %v", err)
	}
}
