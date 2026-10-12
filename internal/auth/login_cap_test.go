package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gatedVerify replaces s.verify. Password "slow" signals entered, then waits
// for the gate; "right" succeeds; anything else fails at once. maxSeen is the
// most checks seen running at once.
type gatedVerify struct {
	entered          chan struct{}
	gate             chan struct{}
	once             sync.Once
	running, maxSeen atomic.Int32
}

func newGatedVerify(t *testing.T, s *Service) *gatedVerify {
	g := &gatedVerify{entered: make(chan struct{}, 64), gate: make(chan struct{})}
	t.Cleanup(g.open) // registered after newService's cleanup, so it runs first
	s.verify = func(password, _ string) (bool, error) {
		n := g.running.Add(1)
		defer g.running.Add(-1)
		for m := g.maxSeen.Load(); n > m && !g.maxSeen.CompareAndSwap(m, n); m = g.maxSeen.Load() {
		}
		switch password {
		case "slow":
			g.entered <- struct{}{}
			<-g.gate
			return false, nil
		case "right":
			return true, nil
		}
		return false, nil
	}
	return g
}

func (g *gatedVerify) open() { g.once.Do(func() { close(g.gate) }) }

// holdSlots starts maxConcurrentHashes slow logins and waits until every one
// is inside verify. Results arrive on the returned channel.
func holdSlots(s *Service, g *gatedVerify) chan error {
	held := make(chan error, maxConcurrentHashes)
	for i := 0; i < maxConcurrentHashes; i++ {
		go func(i int) {
			_, err := s.Authenticate(context.Background(), fmt.Sprintf("ghost%d", i), "slow", fmt.Sprintf("10.0.0.%d", i))
			held <- err
		}(i)
	}
	for i := 0; i < maxConcurrentHashes; i++ {
		<-g.entered
	}
	return held
}

func TestSuccessfulLoginsDoNotCountAgainstIP(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	for i := 0; i < maxIPFailures-1; i++ {
		if _, err := s.Authenticate(ctx, fmt.Sprintf("nobody%d", i), "bad", "7.7.7.7"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Authenticate(ctx, "alice", "password-one", "7.7.7.7"); err != nil {
			t.Fatalf("success %d: %v", i, err)
		}
	}
	if _, err := s.Authenticate(ctx, "nobody-last", "bad", "7.7.7.7"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("last failure: %v", err)
	}
	if _, err := s.Authenticate(ctx, "alice", "password-one", "7.7.7.7"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after the IP's failures are used up: %v", err)
	}
}

func TestSuccessfulLoginResetsPairFailures(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	for round := 0; round < 2; round++ {
		for i := 0; i < maxPairFailures-1; i++ {
			if _, err := s.Authenticate(ctx, "alice", "wrong", "1.0.0.1"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("round %d failure %d: %v", round, i, err)
			}
		}
		if _, err := s.Authenticate(ctx, "alice", "password-one", "1.0.0.1"); err != nil {
			t.Fatalf("round %d success: %v", round, err)
		}
	}
}

func TestRefusedAttemptsDoNotExtendTheBlock(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	t0 := *clock
	for i := 0; i < maxPairFailures; i++ {
		_, _ = s.Authenticate(ctx, "alice", "wrong", "9.9.9.9")
	}
	*clock = t0.Add(10 * time.Minute)
	for i := 0; i < maxPairFailures; i++ {
		if _, err := s.Authenticate(ctx, "alice", "password-one", "9.9.9.9"); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("refused attempt %d: %v", i, err)
		}
	}
	*clock = t0.Add(loginWindow + time.Second)
	if _, err := s.Authenticate(ctx, "alice", "password-one", "9.9.9.9"); err != nil {
		t.Fatalf("after the original window: %v", err)
	}
}

func TestUnknownUserStillRunsVerify(t *testing.T) {
	s, _ := newService(t)
	calls := 0
	s.verify = func(password, encoded string) (bool, error) {
		calls++
		return VerifyPassword(password, encoded)
	}
	if _, err := s.Authenticate(context.Background(), "ghost", "whatever", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("verify ran %d times, want 1", calls)
	}
}

func TestConcurrentHashesAreCapped(t *testing.T) {
	s, _ := newService(t)
	g := newGatedVerify(t, s)
	held := holdSlots(s, g)

	const extras = 8
	extra := make(chan error, extras)
	for i := 0; i < extras; i++ {
		go func(i int) {
			_, err := s.Authenticate(context.Background(), fmt.Sprintf("x%d", i), "slow", fmt.Sprintf("10.0.1.%d", i))
			extra <- err
		}(i)
	}
	for i := 0; i < extras; i++ {
		select {
		case err := <-extra:
			if !errors.Is(err, ErrRateLimited) {
				t.Errorf("extra login: %v, want ErrRateLimited", err)
			}
		case <-g.entered:
			t.Fatalf("an extra login started a password check while all %d slots were busy", maxConcurrentHashes)
		}
	}
	g.open()
	for i := 0; i < maxConcurrentHashes; i++ {
		if err := <-held; !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("held login: %v", err)
		}
	}
	if m := g.maxSeen.Load(); m > maxConcurrentHashes {
		t.Errorf("saw %d checks at once, cap is %d", m, maxConcurrentHashes)
	}
	if _, err := s.Authenticate(context.Background(), "ghost-after", "bad", "10.0.2.1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("after the slots were freed: %v", err)
	}
}

func TestAttemptIsCountedBeforeHashing(t *testing.T) {
	if maxConcurrentHashes < 2 {
		t.Fatal("test needs two slots")
	}
	ctx := context.Background()
	t.Run("username", func(t *testing.T) {
		s, _ := newService(t)
		g := newGatedVerify(t, s)
		_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
		for i := 0; i < maxPairFailures-1; i++ {
			if _, err := s.Authenticate(ctx, "alice", "wrong", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("failure %d: %v", i, err)
			}
		}
		done := make(chan error, 1)
		go func() {
			_, err := s.Authenticate(ctx, "alice", "slow", "1.1.1.1")
			done <- err
		}()
		<-g.entered
		if _, err := s.Authenticate(ctx, "alice", "right", "1.1.1.1"); !errors.Is(err, ErrRateLimited) {
			t.Errorf("login while the last allowed attempt is in flight: %v, want ErrRateLimited", err)
		}
		g.open()
		if err := <-done; !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("in-flight login: %v", err)
		}
	})
	t.Run("ip", func(t *testing.T) {
		s, _ := newService(t)
		g := newGatedVerify(t, s)
		for i := 0; i < maxIPFailures-1; i++ {
			if _, err := s.Authenticate(ctx, fmt.Sprintf("ipuser%d", i), "wrong", "5.5.5.5"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("failure %d: %v", i, err)
			}
		}
		done := make(chan error, 1)
		go func() {
			_, err := s.Authenticate(ctx, "slowuser", "slow", "5.5.5.5")
			done <- err
		}()
		<-g.entered
		if _, err := s.Authenticate(ctx, "other", "wrong", "5.5.5.5"); !errors.Is(err, ErrRateLimited) {
			t.Errorf("login while the last allowed attempt is in flight: %v, want ErrRateLimited", err)
		}
		g.open()
		if err := <-done; !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("in-flight login: %v", err)
		}
	})
}

func TestBusyRefusalsAreNotCounted(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	_, _ = s.CreateUser(ctx, "alice", "password-one", RoleUser)
	g := newGatedVerify(t, s)
	held := holdSlots(s, g)
	for i := 0; i < maxPairFailures+1; i++ {
		if _, err := s.Authenticate(ctx, "alice", "wrong", "8.8.8.8"); !errors.Is(err, ErrRateLimited) {
			t.Errorf("busy attempt %d: %v, want ErrRateLimited", i, err)
		}
	}
	g.open()
	for i := 0; i < maxConcurrentHashes; i++ {
		<-held
	}
	if _, err := s.Authenticate(ctx, "alice", "right", "8.8.8.8"); err != nil {
		t.Errorf("login after the busy refusals: %v", err)
	}
}
