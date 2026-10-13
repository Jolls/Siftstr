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

func TestRateLimitPerPairAndIP(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)

	for i := 0; i < maxPairFailures; i++ {
		_, _ = s.Authenticate(ctx, "alice", "bad", "9.9.9.9")
	}
	// Even the right password is refused from the blocked source...
	if _, err := s.Authenticate(ctx, "alice", "password-one", "9.9.9.9"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("per-pair limit: %v", err)
	}
	// ...but not from another one.
	if _, err := s.Authenticate(ctx, "alice", "password-one", "2.2.2.2"); err != nil {
		t.Fatalf("other source: %v", err)
	}
	*clock = clock.Add(loginWindow + time.Second)
	if _, err := s.Authenticate(ctx, "alice", "password-one", "9.9.9.9"); err != nil {
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

func TestNewUsersGetTheConfiguredDefaultTimezone(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	s.SetDefaultTimezone("Europe/Berlin")
	u, err := s.CreateUser(ctx, "berta", "correct horse battery", RoleUser)
	if err != nil || u.Timezone != "Europe/Berlin" {
		t.Fatalf("user = %+v, %v", u, err)
	}
	s.SetDefaultTimezone("Not/AZone") // ignored: the previous default stays
	u, _ = s.CreateUser(ctx, "carl", "correct horse battery", RoleUser)
	if u.Timezone != "Europe/Berlin" {
		t.Fatalf("typo replaced the default: %s", u.Timezone)
	}
}

func TestUserForSessionUpdatesLastSeen(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	token, _, _ := s.NewSession(ctx, u.ID)
	*clock = clock.Add(time.Hour)
	if _, err := s.UserForSession(ctx, token); err != nil {
		t.Fatal(err)
	}
	var got string
	_ = s.db.QueryRowContext(ctx, `SELECT last_seen_at FROM sessions WHERE user_id = ?`, u.ID).Scan(&got)
	if want := clock.UTC().Format(time.RFC3339); got != want {
		t.Fatalf("last_seen_at = %q, want %q", got, want)
	}
}

// A phone used once a day is never cut off, but the absolute limit still holds.
func TestDailyUseKeepsSessionAlive(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	token, _, _ := s.NewSession(ctx, u.ID)
	start := *clock
	for d := 1; d <= 29; d++ {
		*clock = start.Add(time.Duration(d) * 24 * time.Hour)
		if _, err := s.UserForSession(ctx, token); err != nil {
			t.Fatalf("day %d: %v", d, err)
		}
	}
	*clock = start.Add(SessionLifetime)
	if _, err := s.UserForSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("at absolute expiry: %v", err)
	}
}

func TestSetPasswordEndsOnlyThatUsersSessions(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	alice, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	bob, _ := s.CreateUser(ctx, "bob", "password-two", RoleUser)
	at, _, _ := s.NewSession(ctx, alice.ID)
	bt, _, _ := s.NewSession(ctx, bob.ID)
	if err := s.SetPassword(ctx, "alice", "password-new"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForSession(ctx, at); !errors.Is(err, ErrNoSession) {
		t.Fatalf("alice: %v", err)
	}
	if got, err := s.UserForSession(ctx, bt); err != nil || got.ID != bob.ID {
		t.Fatalf("bob: %v", err)
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	start := *clock
	token, _, _ := s.NewSession(ctx, u.ID)
	*clock = start.Add(SessionIdleTimeout + time.Second)
	if _, err := s.UserForSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("idle: %v", err)
	}
}

func TestSessionIdleBoundary(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	start := *clock
	token, _, _ := s.NewSession(ctx, u.ID)
	*clock = start.Add(SessionIdleTimeout - time.Second)
	if _, err := s.UserForSession(ctx, token); err != nil {
		t.Fatalf("just before: %v", err)
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE user_id = ?`, start.Format(time.RFC3339), u.ID)
	*clock = start.Add(SessionIdleTimeout)
	if _, err := s.UserForSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("at boundary: %v", err)
	}
}

func TestIdleTimeoutSlides(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	u, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	start := *clock
	token, _, _ := s.NewSession(ctx, u.ID)
	*clock = start.Add(SessionIdleTimeout - time.Hour)
	if _, err := s.UserForSession(ctx, token); err != nil {
		t.Fatalf("first: %v", err)
	}
	*clock = start.Add(2*SessionIdleTimeout - 2*time.Hour)
	if _, err := s.UserForSession(ctx, token); err != nil {
		t.Fatalf("second: %v", err)
	}
	*clock = clock.Add(SessionIdleTimeout)
	if _, err := s.UserForSession(ctx, token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after idle: %v", err)
	}
}

func TestEndAllSessionsIsScopedToUser(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	alice, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	bob, _ := s.CreateUser(ctx, "bob", "password-two", RoleUser)
	a1, _, _ := s.NewSession(ctx, alice.ID)
	a2, _, _ := s.NewSession(ctx, alice.ID)
	bt, _, _ := s.NewSession(ctx, bob.ID)
	n, err := s.EndAllSessions(ctx, alice.ID)
	if err != nil || n != 2 {
		t.Fatalf("EndAllSessions = %d, %v", n, err)
	}
	for _, tok := range []string{a1, a2} {
		if _, err := s.UserForSession(ctx, tok); !errors.Is(err, ErrNoSession) {
			t.Errorf("alice session survived: %v", err)
		}
	}
	if got, err := s.UserForSession(ctx, bt); err != nil || got.ID != bob.ID {
		t.Fatalf("bob: %v", err)
	}
	if _, err := s.EndAllSessions(ctx, ""); err == nil {
		t.Error("empty user ID should error")
	}
	var c int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, bob.ID).Scan(&c)
	if c != 1 {
		t.Fatalf("bob sessions = %d", c)
	}
}

func TestListSessionsScopedAndFiltered(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	alice, _ := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	bob, _ := s.CreateUser(ctx, "bob", "password-two", RoleUser)
	start := *clock
	_, _, _ = s.NewSession(ctx, alice.ID) // a1: idle by the time we list
	_, _, _ = s.NewSession(ctx, bob.ID)
	*clock = start.Add(8 * 24 * time.Hour)
	a2, _, _ := s.NewSession(ctx, alice.ID)
	*clock = clock.Add(time.Hour)
	got, err := s.ListSessions(ctx, alice.ID, a2)
	if err != nil || len(got) != 1 || !got[0].Current || !got[0].LastSeenAt.Equal(start.Add(8*24*time.Hour)) {
		t.Fatalf("%+v %v", got, err)
	}
	got, _ = s.ListSessions(ctx, alice.ID, "other")
	if len(got) != 1 || got[0].Current {
		t.Fatalf("other token: %+v", got)
	}
}

func TestShortExistingPasswordStillAuthenticates(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "alice", "password-one", RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := HashPassword("eight-ch")
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, h, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "alice", "eight-ch", "1.1.1.1"); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
}

func TestPasswordMinimumIsTwelve(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "bob", "elevenchars", RoleUser); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("11 chars: %v", err)
	}
	if _, err := s.CreateUser(ctx, "bob", "twelve-chars", RoleUser); err != nil {
		t.Errorf("12 chars: %v", err)
	}
	if err := s.SetPassword(ctx, "bob", "elevenchars"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("set 11 chars: %v", err)
	}
	if !strings.Contains(ErrWeakPassword.Error(), "12") {
		t.Errorf("message = %q", ErrWeakPassword)
	}
	s2, _ := newService(t)
	if _, err := s2.Bootstrap(ctx, "admin", "elevenchars"); !errors.Is(err, ErrWeakPassword) {
		t.Errorf("bootstrap 11 chars: %v", err)
	}
	var n int
	_ = s2.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 0 {
		t.Errorf("%d users created", n)
	}
}
