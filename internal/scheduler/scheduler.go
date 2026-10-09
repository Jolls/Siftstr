// Package scheduler runs Siftstr's in-app timers (ingest, release, janitor).
//
// Jobs are either per-user or instance-wide. A per-user job runs once per
// user per tick, each in its own goroutine, so one user's slow or failing
// upstream never blocks another user's run.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Scope says whether a job runs once per user or once per instance.
type Scope int

const (
	// PerUser jobs run for every enabled user. Run receives that user's ID.
	PerUser Scope = iota
	// Instance jobs run once per tick. Run receives an empty user ID.
	Instance
)

// DefaultTimeout bounds a single run when Job.Timeout is zero.
const DefaultTimeout = 5 * time.Minute

// Job is a recurring task.
type Job struct {
	Name       string
	Interval   time.Duration
	Scope      Scope
	Timeout    time.Duration // per run; DefaultTimeout if zero
	RunOnStart bool          // run once immediately instead of waiting a full interval
	Run        func(ctx context.Context, userID string) error
}

// UserLister returns the IDs of users whose jobs should run (not disabled).
type UserLister func(ctx context.Context) ([]string, error)

// Scheduler owns the registered jobs.
type Scheduler struct {
	users UserLister
	log   *slog.Logger

	mu      sync.Mutex
	jobs    []Job
	started bool
	running map[string]bool // job+user currently in flight
}

// New returns a Scheduler. log may be nil.
func New(users UserLister, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{users: users, log: log, running: make(map[string]bool)}
}

// Register adds a job. It must be called before Run.
func (s *Scheduler) Register(j Job) error {
	switch {
	case j.Name == "":
		return errors.New("scheduler: job needs a name")
	case j.Interval <= 0:
		return fmt.Errorf("scheduler: job %q needs a positive interval", j.Name)
	case j.Run == nil:
		return fmt.Errorf("scheduler: job %q has no Run func", j.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("scheduler: cannot register %q after Run", j.Name)
	}
	for _, other := range s.jobs {
		if other.Name == j.Name {
			return fmt.Errorf("scheduler: duplicate job %q", j.Name)
		}
	}
	s.jobs = append(s.jobs, j)
	return nil
}

// Run starts every job's timer and blocks until ctx is cancelled and all
// in-flight runs have returned.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.started = true
	jobs := append([]Job(nil), s.jobs...)
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.loop(ctx, j, &wg)
		}()
	}
	wg.Wait()
}

func (s *Scheduler) loop(ctx context.Context, j Job, wg *sync.WaitGroup) {
	if j.RunOnStart {
		s.dispatch(ctx, j, wg)
	}
	t := time.NewTicker(j.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.dispatch(ctx, j, wg)
		}
	}
}

// dispatch starts the job's runs for this tick without waiting for them.
func (s *Scheduler) dispatch(ctx context.Context, j Job, wg *sync.WaitGroup) {
	if j.Scope == Instance {
		s.start(ctx, j, "", wg)
		return
	}
	ids, err := s.users(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error("scheduler: list users", "job", j.Name, "err", err)
		}
		return
	}
	for _, id := range ids {
		s.start(ctx, j, id, wg)
	}
}

// start launches one run unless the previous run for the same job and user
// is still going, so a slow run never stacks up behind itself.
func (s *Scheduler) start(ctx context.Context, j Job, userID string, wg *sync.WaitGroup) {
	key := j.Name + "\x00" + userID
	s.mu.Lock()
	if s.running[key] {
		s.mu.Unlock()
		s.log.Warn("scheduler: previous run still going, skipping", "job", j.Name, "user", userID)
		return
	}
	s.running[key] = true
	s.mu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.running, key)
			s.mu.Unlock()
		}()
		s.runOne(ctx, j, userID)
	}()
}

// runOne runs the job with a timeout and turns errors and panics into log
// lines. Errors are logged by job and user ID only; Run must not put
// credentials into the errors it returns.
func (s *Scheduler) runOne(ctx context.Context, j Job, userID string) {
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			s.log.Error("scheduler: job panicked", "job", j.Name, "user", userID, "panic", fmt.Sprint(r))
		}
	}()
	if err := j.Run(ctx, userID); err != nil && ctx.Err() == nil {
		s.log.Error("scheduler: job failed", "job", j.Name, "user", userID, "err", err)
	}
}
