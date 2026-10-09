package auth

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/store"
)

func newService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(st.DB(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }
	return s, &clock
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$") {
		t.Fatalf("hash = %q", h)
	}
	if ok, _ := VerifyPassword("correct horse", h); !ok {
		t.Error("right password rejected")
	}
	if ok, _ := VerifyPassword("wrong", h); ok {
		t.Error("wrong password accepted")
	}
	if _, err := VerifyPassword("x", "garbage"); err == nil {
		t.Error("malformed hash should error")
	}
}

func TestCreateAndAuthenticate(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := s.CreateUser(ctx, "Alice", "short", RoleUser); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	u, err := s.CreateUser(ctx, "Alice", "password-one", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "alice", "password-two", RoleUser); !errors.Is(err, ErrUserExists) {
		t.Fatalf("duplicate (case-insensitive): %v", err)
	}
	got, err := s.Authenticate(ctx, " ALICE ", "password-one", "1.1.1.1")
	if err != nil || got.ID != u.ID {
		t.Fatalf("login: %v", err)
	}
	if _, err := s.Authenticate(ctx, "alice", "nope", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := s.Authenticate(ctx, "ghost", "password-one", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user must look like a wrong password: %v", err)
	}
}

func TestDisabledUserCannotLogIn(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	u, _ := s.CreateUser(ctx, "bob", "password-one", RoleUser)
	token, _, _ := s.NewSession(ctx, u.ID)
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET disabled = 1 WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "bob", "password-one", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login: %v", err)
	}
	if _, err := s.UserForSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("existing session: %v", err)
	}
}

func TestRateLimitPerUserAndIP(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)

	for i := 0; i < maxUserFailures; i++ {
		_, _ = s.Authenticate(ctx, "alice", "bad", "9.9.9.9")
	}
	// Even the right password is refused while blocked.
	if _, err := s.Authenticate(ctx, "alice", "password-one", "2.2.2.2"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("per-username limit: %v", err)
	}
	*clock = clock.Add(loginWindow + time.Second)
	if _, err := s.Authenticate(ctx, "alice", "password-one", "2.2.2.2"); err != nil {
		t.Fatalf("after window: %v", err)
	}

	// One IP guessing many usernames gets blocked.
	for i := 0; i < maxIPFailures; i++ {
		_, _ = s.Authenticate(ctx, "user"+string(rune('a'+i)), "bad", "6.6.6.6")
	}
	if _, err := s.Authenticate(ctx, "alice", "password-one", "6.6.6.6"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("per-IP limit: %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)

	token, _, err := s.NewSession(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Only a hash of the token is stored.
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE id = ?`, token).Scan(&n)
	if n != 0 {
		t.Fatal("raw token stored in database")
	}
	got, err := s.UserForSession(ctx, token)
	if err != nil || got.ID != u.ID {
		t.Fatalf("lookup: %v", err)
	}
	if _, err := s.UserForSession(ctx, "forged"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("forged token: %v", err)
	}

	*clock = clock.Add(SessionLifetime + time.Second)
	if _, err := s.UserForSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired: %v", err)
	}

	*clock = time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)
	token2, _, _ := s.NewSession(ctx, u.ID)
	_ = s.EndSession(ctx, token2)
	if _, err := s.UserForSession(ctx, token2); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after logout: %v", err)
	}
}

func TestSetPasswordEndsSessions(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	token, _, _ := s.NewSession(ctx, u.ID)
	if err := s.SetPassword(ctx, "alice", "password-two"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatal("session survived a password change")
	}
	if _, err := s.Authenticate(ctx, "alice", "password-two", "1.1.1.1"); err != nil {
		t.Fatalf("new password: %v", err)
	}
	if err := s.SetPassword(ctx, "ghost", "password-two"); !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("unknown user: %v", err)
	}
}

func TestBootstrapOnlyWhenEmpty(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if made, _ := s.Bootstrap(ctx, "", ""); made {
		t.Fatal("bootstrap without credentials created a user")
	}
	if made, err := s.Bootstrap(ctx, "admin", "password-one"); !made || err != nil {
		t.Fatalf("first bootstrap: %v %v", made, err)
	}
	if made, _ := s.Bootstrap(ctx, "other", "password-two"); made {
		t.Fatal("bootstrap ran with users present")
	}
	u, err := s.Authenticate(ctx, "admin", "password-one", "1.1.1.1")
	if err != nil || u.Role != RoleAdmin {
		t.Fatalf("admin login: %v %+v", err, u)
	}
}

// CSRF tokens are bound to one session.
func TestCSRF(t *testing.T) {
	s, _ := newService(t)
	a := s.CSRFToken("session-a")
	if !s.CheckCSRF("session-a", a) {
		t.Error("valid token rejected")
	}
	if s.CheckCSRF("session-b", a) {
		t.Error("token accepted for a different session")
	}
	if s.CheckCSRF("session-a", "") || s.CheckCSRF("", a) {
		t.Error("empty values accepted")
	}
}
