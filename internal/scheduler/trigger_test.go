package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// runBG runs the scheduler in the background; stop cancels it and waits.
func runBG(t *testing.T, s *Scheduler) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// recorder counts runs per user and can block them on a gate.
type recorder struct {
	mu     sync.Mutex
	calls  []string
	starts []time.Time
	gate   chan struct{} // if set, the first run per user (or only's) waits on it
	only   string        // if set, only this user's runs wait
	begun  chan string
	waited map[string]bool
}

func (r *recorder) run(ctx context.Context, user string) error {
	r.mu.Lock()
	r.calls = append(r.calls, user)
	r.starts = append(r.starts, time.Now())
	gate, begun := r.gate, r.begun
	block := gate != nil && (r.only == "" || r.only == user)
	if r.waited == nil {
		r.waited = map[string]bool{}
	}
	if r.waited[user] { // only the first run per user blocks
		block = false
	}
	r.waited[user] = true
	r.mu.Unlock()
	if begun != nil {
		select {
		case begun <- user:
		default:
		}
	}
	if block {
		select {
		case <-gate:
		case <-ctx.Done():
		}
	}
	return nil
}

func (r *recorder) count(user string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if user == "" || c == user {
			n++
		}
	}
	return n
}

func hourly(name string, r *recorder) Job {
	return Job{Name: name, Interval: time.Hour, Scope: PerUser, Run: r.run}
}

func TestTriggerRunsOnlyThatUser(t *testing.T) {
	s := New(users("u1", "u2"), quiet())
	r := &recorder{}
	if err := s.Register(hourly("j", r)); err != nil {
		t.Fatal(err)
	}
	defer runBG(t, s)()
	if err := s.Trigger("j", "u1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return r.count("") == 1 })
	time.Sleep(50 * time.Millisecond)
	if r.count("u1") != 1 || r.count("") != 1 {
		t.Fatalf("calls = %v", r.calls)
	}
}

func TestTriggerDoesNotBlockTheCaller(t *testing.T) {
	s := New(users("u1"), quiet())
	r := &recorder{gate: make(chan struct{}), begun: make(chan string, 1)}
	s.Register(hourly("j", r))
	defer runBG(t, s)()
	s.Trigger("j", "u1")
	select {
	case <-r.begun:
	case <-time.After(3 * time.Second):
		t.Fatal("run never started")
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			s.Trigger("j", "u1")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Trigger blocked")
	}
	close(r.gate)
}

func TestTwoTriggersDuringARunGiveOneRerun(t *testing.T) {
	s := New(users("u1"), quiet())
	s.gap = 0
	r := &recorder{gate: make(chan struct{}), begun: make(chan string, 1)}
	s.Register(hourly("j", r))
	defer runBG(t, s)()
	s.Trigger("j", "u1")
	select {
	case <-r.begun:
	case <-time.After(3 * time.Second):
		t.Fatal("run never started")
	}
	s.Trigger("j", "u1")
	s.Trigger("j", "u1")
	time.Sleep(30 * time.Millisecond)
	close(r.gate)
	waitFor(t, func() bool { return r.count("") >= 2 })
	time.Sleep(100 * time.Millisecond)
	if n := r.count(""); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

func TestTriggerInsideGapIsDelayedNotDropped(t *testing.T) {
	s := New(users("u1"), quiet())
	s.gap = 150 * time.Millisecond
	r := &recorder{}
	j := hourly("j", r)
	j.RunOnStart = true
	s.Register(j)
	defer runBG(t, s)()
	waitFor(t, func() bool { return r.count("") == 1 })
	s.Trigger("j", "u1")
	waitFor(t, func() bool { return r.count("") == 2 })
	r.mu.Lock()
	d := r.starts[1].Sub(r.starts[0])
	r.mu.Unlock()
	if d < 130*time.Millisecond {
		t.Fatalf("second start after %v, want >= 130ms", d)
	}
}

func TestOutboxAndHighlightsSkipTheGap(t *testing.T) {
	s := New(users("u1"), quiet())
	s.gap = time.Hour
	recs := map[string]*recorder{JobOutbox: {}, JobHighlights: {}, "ingest": {}}
	for name, r := range recs {
		s.Register(hourly(name, r))
	}
	defer runBG(t, s)()
	for name, r := range recs {
		s.Trigger(name, "u1")
		waitFor(t, func() bool { return r.count("") == 1 })
		s.Trigger(name, "u1")
	}
	waitFor(t, func() bool { return recs[JobOutbox].count("") == 2 && recs[JobHighlights].count("") == 2 })
	time.Sleep(200 * time.Millisecond)
	if n := recs["ingest"].count(""); n != 1 {
		t.Fatalf("ingest runs = %d, want 1", n)
	}
}

func TestTriggerBeforeRunIsHandled(t *testing.T) {
	s := New(users("u1"), quiet())
	r := &recorder{}
	s.Register(hourly("j", r))
	if err := s.Trigger("j", "u1"); err != nil {
		t.Fatal(err)
	}
	defer runBG(t, s)()
	waitFor(t, func() bool { return r.count("") == 1 })
}

func TestTriggerErrors(t *testing.T) {
	s := New(users("u1"), quiet())
	r := &recorder{}
	s.Register(hourly("j", r))
	s.Register(Job{Name: "inst", Interval: time.Hour, Scope: Instance, Run: r.run})
	if err := s.Trigger("nope", "u1"); !errors.Is(err, ErrUnknownJob) {
		t.Errorf("unknown: %v", err)
	}
	if err := s.Trigger("inst", "u1"); !errors.Is(err, ErrInstanceJob) {
		t.Errorf("instance: %v", err)
	}
	if err := s.Trigger("j", ""); !errors.Is(err, ErrNoUser) {
		t.Errorf("no user: %v", err)
	}
}

func TestTriggerIsolation(t *testing.T) {
	s := New(users("u1", "u2"), quiet())
	s.gap = 0
	r := &recorder{gate: make(chan struct{}), only: "u1", begun: make(chan string, 4)}
	s.Register(hourly("j", r))
	defer runBG(t, s)()
	s.Trigger("j", "u1")
	select {
	case u := <-r.begun:
		if u != "u1" {
			t.Fatalf("started %s", u)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("u1 never started")
	}
	s.Trigger("j", "u2")
	waitFor(t, func() bool { return r.count("u2") == 1 })
	s.Trigger("j", "u1")
	time.Sleep(50 * time.Millisecond)
	if n := r.count("u1"); n != 1 {
		t.Fatalf("u1 runs while blocked = %d", n)
	}
	close(r.gate)
	waitFor(t, func() bool { return r.count("u1") == 2 })
	if n := r.count("u2"); n != 1 {
		t.Fatalf("u2 runs = %d", n)
	}
}

func TestTriggeredSkipLogsDebugNotWarn(t *testing.T) {
	buf := &syncBuf{}
	s := New(users("u1"), slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	s.gap = 0
	r := &recorder{gate: make(chan struct{}), begun: make(chan string, 1)}
	s.Register(hourly("j", r))
	defer runBG(t, s)()
	s.Trigger("j", "u1")
	select {
	case <-r.begun:
	case <-time.After(3 * time.Second):
		t.Fatal("run never started")
	}
	s.Trigger("j", "u1")
	waitFor(t, func() bool { return strings.Contains(buf.String(), "will run again") })
	close(r.gate)
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || strings.Contains(out, "level=WARN") {
		t.Fatalf("log = %s", out)
	}
}

func TestTriggerAfterCancelDoesNothing(t *testing.T) {
	s := New(users("u1"), quiet())
	r := &recorder{}
	s.Register(hourly("j", r))
	stop := runBG(t, s)
	stop()
	if err := s.Trigger("j", "u1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := r.count(""); n != 0 {
		t.Fatalf("runs = %d", n)
	}
}

func TestTickSkipStillWarns(t *testing.T) {
	buf := &syncBuf{}
	s := New(users("u1"), slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	r := &recorder{gate: make(chan struct{})}
	s.Register(Job{Name: "j", Interval: 10 * time.Millisecond, Scope: PerUser, RunOnStart: true, Run: r.run})
	runFor(t, s, func() bool { return strings.Contains(buf.String(), "previous run still going") })
	close(r.gate)
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestTriggerForDisabledUserDoesNothing(t *testing.T) {
	s := New(users("u1"), quiet())
	r := &recorder{}
	s.Register(hourly("j", r))
	defer runBG(t, s)()
	s.Trigger("j", "gone")
	s.Trigger("j", "u1")
	waitFor(t, func() bool { return r.count("u1") == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := r.count("gone"); n != 0 {
		t.Fatalf("disabled user ran %d times", n)
	}
}
