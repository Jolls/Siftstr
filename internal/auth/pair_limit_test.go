package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPairLimitBlocksSameSource(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	for i := 0; i < 5; i++ { // 5 is the per-pair limit
		if _, err := s.Authenticate(ctx, "alice", "bad", "9.9.9.9"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	if _, err := s.Authenticate(ctx, "alice", "password-one", "9.9.9.9"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("same source after the limit: %v", err)
	}
	*clock = clock.Add(loginWindow + time.Second)
	if _, err := s.Authenticate(ctx, "alice", "password-one", "9.9.9.9"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

func TestLongUsernameNeverMatches(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	name := strings.Repeat("a", 64)
	if _, err := s.CreateUser(ctx, name, "password-one", RoleUser); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, name+"b", "password-one", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("longer name: %v", err)
	}
	if _, err := s.Authenticate(ctx, name, "password-one", "1.1.1.1"); err != nil {
		t.Fatalf("exact name: %v", err)
	}
}
