package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func users(ids ...string) UserLister {
	return func(context.Context) ([]string, error) { return ids, nil }
}

// runFor runs the scheduler until cond is true or the deadline passes.
func runFor(t *testing.T, s *Scheduler, cond func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRegisterValidation(t *testing.T) {
	s := New(users(), quiet())
	ok := func(context.Context, string) error { return nil }
	for name, j := range map[string]Job{
		"no name":     {Interval: time.Second, Run: ok},
		"no interval": {Name: "a", Run: ok},
		"no run":      {Name: "a", Interval: time.Second},
	} {
		if err := s.Register(j); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := s.Register(Job{Name: "a", Interval: time.Second, Run: ok}); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(Job{Name: "a", Interval: time.Second, Run: ok}); err == nil {
		t.Error("duplicate name accepted")
	}
}

func TestPerUserJobRunsForEachUser(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	s := New(users("u1", "u2"), quiet())
	_ = s.Register(Job{Name: "ingest", Interval: 10 * time.Millisecond, RunOnStart: true,
		Run: func(_ context.Context, id string) error {
			mu.Lock()
			seen[id]++
			mu.Unlock()
			return nil
		}})
	runFor(t, s, func() bool { mu.Lock(); defer mu.Unlock(); return seen["u1"] >= 2 && seen["u2"] >= 2 })
	mu.Lock()
	defer mu.Unlock()
	if seen["u1"] < 2 || seen["u2"] < 2 {
		t.Fatalf("runs = %v", seen)
	}
}

func TestInstanceJobRunsOnceWithNoUser(t *testing.T) {
	var calls atomic.Int32
	var gotUser atomic.Value
	s := New(users("u1", "u2"), quiet())
	_ = s.Register(Job{Name: "janitor", Scope: Instance, Interval: time.Hour, RunOnStart: true,
		Run: func(_ context.Context, id string) error {
			gotUser.Store(id)
			calls.Add(1)
			return nil
		}})
	runFor(t, s, func() bool { return calls.Load() >= 1 })
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 1 || gotUser.Load() != "" {
		t.Fatalf("calls = %d, user = %q", calls.Load(), gotUser.Load())
	}
}

// One user's hung job must not stop another user's job from running.
func TestSlowUserDoesNotBlockOthers(t *testing.T) {
	var fastRuns atomic.Int32
	release := make(chan struct{})
	defer close(release)
	s := New(users("slow", "fast"), quiet())
	_ = s.Register(Job{Name: "ingest", Interval: 10 * time.Millisecond, RunOnStart: true,
		Run: func(ctx context.Context, id string) error {
			if id == "slow" {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return nil
			}
			fastRuns.Add(1)
			return nil
		}})
	runFor(t, s, func() bool { return fastRuns.Load() >= 3 })
	if fastRuns.Load() < 3 {
		t.Fatalf("fast user ran %d times", fastRuns.Load())
	}
}

func TestFailureAndPanicDoNotStopTheLoop(t *testing.T) {
	var calls atomic.Int32
	s := New(users("u1"), quiet())
	_ = s.Register(Job{Name: "ingest", Interval: 10 * time.Millisecond, RunOnStart: true,
		Run: func(context.Context, string) error {
			switch calls.Add(1) {
			case 1:
				return errors.New("upstream down")
			case 2:
				panic("boom")
			}
			return nil
		}})
	runFor(t, s, func() bool { return calls.Load() >= 4 })
	if calls.Load() < 4 {
		t.Fatalf("loop stopped after failure/panic: %d calls", calls.Load())
	}
}

// A run that outlasts its interval is skipped, never stacked.
func TestNoOverlapForSameJobAndUser(t *testing.T) {
	var active, maxActive, calls atomic.Int32
	s := New(users("u1"), quiet())
	_ = s.Register(Job{Name: "ingest", Interval: 5 * time.Millisecond, RunOnStart: true,
		Run: func(context.Context, string) error {
			n := active.Add(1)
			for {
				m := maxActive.Load()
				if n <= m || maxActive.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			active.Add(-1)
			calls.Add(1)
			return nil
		}})
	runFor(t, s, func() bool { return calls.Load() >= 2 })
	if maxActive.Load() != 1 {
		t.Fatalf("max concurrent runs = %d", maxActive.Load())
	}
}

func TestTimeoutCancelsRun(t *testing.T) {
	var timedOut atomic.Bool
	s := New(users("u1"), quiet())
	_ = s.Register(Job{Name: "ingest", Interval: time.Hour, RunOnStart: true, Timeout: 20 * time.Millisecond,
		Run: func(ctx context.Context, _ string) error {
			<-ctx.Done()
			timedOut.Store(true)
			return ctx.Err()
		}})
	runFor(t, s, timedOut.Load)
	if !timedOut.Load() {
		t.Fatal("run was not cancelled by its timeout")
	}
}

func TestListUsersErrorIsSurvived(t *testing.T) {
	var lists, runs atomic.Int32
	s := New(func(context.Context) ([]string, error) {
		if lists.Add(1) == 1 {
			return nil, errors.New("db busy")
		}
		return []string{"u1"}, nil
	}, quiet())
	_ = s.Register(Job{Name: "ingest", Interval: 10 * time.Millisecond, RunOnStart: true,
		Run: func(context.Context, string) error { runs.Add(1); return nil }})
	runFor(t, s, func() bool { return runs.Load() >= 1 })
	if runs.Load() < 1 {
		t.Fatal("scheduler never recovered from a user-list error")
	}
}

func TestRegisterAfterRunRejected(t *testing.T) {
	s := New(users(), quiet())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.Run(ctx)
	if err := s.Register(Job{Name: "late", Interval: time.Second, Run: func(context.Context, string) error { return nil }}); err == nil {
		t.Error("register after Run accepted")
	}
}
