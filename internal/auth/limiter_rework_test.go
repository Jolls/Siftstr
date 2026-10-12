package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// recordSleeps replaces s.sleep with a recorder that never waits.
func recordSleeps(s *Service) *[]time.Duration {
	var slept []time.Duration
	s.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	return &slept
}

func TestFailuresFromOneSourceDoNotLockUserOut(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	for i := 0; i < 5; i++ {
		_, _ = s.Authenticate(ctx, "alice", "bad", "9.9.9.9")
	}
	if _, err := s.Authenticate(ctx, "alice", "password-one", "2.2.2.2"); err != nil {
		t.Fatalf("login from another source: %v", err)
	}
}

func TestUsernameDelayAcrossSources(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	slept := recordSleeps(s)
	for i := 0; i < 16; i++ {
		if _, err := s.Authenticate(ctx, "alice", "bad", fmt.Sprintf("198.51.100.%d", i)); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	s2 := time.Second
	want := []time.Duration{s2, 2 * s2, 4 * s2, 8 * s2, 16 * s2, 30 * s2}
	if !slices.Equal(*slept, want) {
		t.Fatalf("delays = %v, want %v", *slept, want)
	}
	if _, err := s.Authenticate(ctx, "alice", "password-one", "203.0.113.1"); err != nil {
		t.Fatalf("real login during the attack: %v", err)
	}
	if len(*slept) != 7 || (*slept)[6] != 30*time.Second {
		t.Fatalf("delays after the real login = %v", *slept)
	}
}

func TestBlockedAttemptsDoNotSleep(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	slept := recordSleeps(s)
	for i := 0; i < 10; i++ {
		_, _ = s.Authenticate(ctx, "alice", "bad", fmt.Sprintf("198.51.100.%d", i))
	}
	for i := 0; i < 5; i++ {
		_, _ = s.Authenticate(ctx, "alice", "bad", "203.0.113.9")
	}
	s2 := time.Second
	if want := []time.Duration{s2, 2 * s2, 4 * s2, 8 * s2, 16 * s2}; !slices.Equal(*slept, want) {
		t.Fatalf("delays = %v, want %v", *slept, want)
	}
	if _, err := s.Authenticate(ctx, "alice", "password-one", "203.0.113.9"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("blocked pair: %v", err)
	}
	if len(*slept) != 5 {
		t.Fatalf("a blocked attempt slept: %v", *slept)
	}
}

func TestDelayHonorsContextAndCountsNothing(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	recordSleeps(s)
	for i := 0; i < 10; i++ {
		_, _ = s.Authenticate(ctx, "alice", "bad", fmt.Sprintf("198.51.100.%d", i))
	}
	s.sleep = sleepCtx
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Authenticate(cctx, "alice", "password-one", "203.0.113.1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := s.byUser.count("alice", *clock); n != 10 {
		t.Errorf("username count = %d, want 10", n)
	}
	if n := s.byPair.count("alice\x00203.0.113.1", *clock); n != 0 {
		t.Errorf("pair count = %d, want 0", n)
	}
}

func TestUserDelaySchedule(t *testing.T) {
	s2 := time.Second
	for n, want := range map[int]time.Duration{0: 0, 9: 0, 10: s2, 11: 2 * s2, 12: 4 * s2, 13: 8 * s2, 14: 16 * s2, 15: 30 * s2, 100: 30 * s2} {
		if got := userDelay(n); got != want {
			t.Errorf("userDelay(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestIPKey(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.1":              "192.0.2.1",
		"::ffff:192.0.2.1":       "192.0.2.1",
		"2001:db8:1:2:aaaa::1":   "2001:db8:1:2::/64",
		"fe80::1%eth0":           "fe80::/64",
		"not-an-ip":              "not-an-ip",
		strings.Repeat("x", 100): strings.Repeat("x", 64),
	} {
		if got := ipKey(in); got != want {
			t.Errorf("ipKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIPv6ClientsGroupedBy64(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	for i := 0; i < 20; i++ {
		_, _ = s.Authenticate(ctx, fmt.Sprintf("nobody%d", i), "bad", fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	if _, err := s.Authenticate(ctx, "alice", "password-one", "2001:db8:1:2::ffff"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("same /64: %v", err)
	}
	if _, err := s.Authenticate(ctx, "alice", "password-one", "2001:db8:1:3::1"); err != nil {
		t.Fatalf("other /64: %v", err)
	}
}

func TestUsernameCappedForLimiterKeys(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := s.Authenticate(ctx, strings.Repeat("A", 1<<20), "bad", "192.0.2.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
	for k := range s.byUser.fails {
		if len(k) > maxUsernameLen {
			t.Errorf("username key is %d bytes", len(k))
		}
	}
	for k := range s.byPair.fails {
		if len(k) > maxUsernameLen+1+len("192.0.2.1") {
			t.Errorf("pair key is %d bytes", len(k))
		}
	}
}

func TestCreateUserRejectsLongUsername(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := s.CreateUser(ctx, strings.Repeat("a", 65), "password-one", RoleUser); !errors.Is(err, ErrUsernameTooLong) {
		t.Fatalf("65 bytes: %v", err)
	}
	if _, err := s.CreateUser(ctx, strings.Repeat("b", 64), "password-one", RoleUser); err != nil {
		t.Fatalf("64 bytes: %v", err)
	}
}
