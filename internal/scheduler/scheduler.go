// Package scheduler runs Siftstr's in-app timers (ingest, release, outbox, highlights, janitor, backup).
//
// Jobs are either per-user or instance-wide. A per-user job runs once per
// user per tick, each in its own goroutine, so one user's slow or failing
// upstream never blocks another user's run.
//
// A per-user job can also be triggered for one user (Trigger), for example
// after a settings save. The job's own loop starts every run, so a job never
// overlaps itself for a user.
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

// jobState is a registered job plus its trigger queue.
type jobState struct {
	Job
	trig    chan struct{}       // cap 1: wakes the job's loop
	pending map[string]struct{} // users waiting for a triggered run; guarded by Scheduler.mu
}

func (j *jobState) signal() {
	select {
	case j.trig <- struct{}{}:
	default:
	}
}

// Scheduler owns the registered jobs.
type Scheduler struct {
	users UserLister
	log   *slog.Logger

	mu        sync.Mutex
	jobs      []*jobState
	byName    map[string]*jobState
	started   bool
	running   map[string]bool      // job+user currently in flight
	dirty     map[string]bool      // job+user: run once more when the current run ends
	lastStart map[string]time.Time // job+user: last start, timer or triggered
	gap       time.Duration        // MinTriggerGap; tests shorten it
}

// New returns a Scheduler. log may be nil.
func New(users UserLister, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{users: users, log: log, running: make(map[string]bool), byName: make(map[string]*jobState),
		dirty: make(map[string]bool), lastStart: make(map[string]time.Time), gap: MinTriggerGap}
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
	if s.byName[j.Name] != nil {
		return fmt.Errorf("scheduler: duplicate job %q", j.Name)
	}
	js := &jobState{Job: j, trig: make(chan struct{}, 1), pending: map[string]struct{}{}}
	s.jobs = append(s.jobs, js)
	s.byName[j.Name] = js
	return nil
}

// Run starts every job's timer and blocks until ctx is cancelled and all
// in-flight runs have returned.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	s.started = true
	jobs := append([]*jobState(nil), s.jobs...)
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

func (s *Scheduler) loop(ctx context.Context, j *jobState, wg *sync.WaitGroup) {
	if j.RunOnStart {
		s.dispatch(ctx, j, wg)
	}
	t := time.NewTicker(j.Interval)
	defer t.Stop()
	delay := time.NewTimer(time.Hour) // re-armed for triggers waiting out the gap
	delay.Stop()
	defer delay.Stop()
	for {
		var wait time.Duration
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.dispatch(ctx, j, wg)
			continue
		case <-j.trig:
			wait = s.drain(ctx, j, wg)
		case <-delay.C:
			wait = s.drain(ctx, j, wg)
		}
		if wait > 0 {
			delay.Reset(wait)
		} else {
			delay.Stop()
		}
	}
}

// dispatch starts the job's runs for this tick without waiting for them.
func (s *Scheduler) dispatch(ctx context.Context, j *jobState, wg *sync.WaitGroup) {
	if j.Scope == Instance {
		s.start(ctx, j, "", wg, false)
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
		s.start(ctx, j, id, wg, false)
	}
}

func runKey(name, userID string) string { return name + "\x00" + userID }

// Trigger asks for one run of the per-user job name for userID, soon, without
// waiting for its timer. It never blocks: the job's loop starts the run with
// the scheduler's context and the job's timeout. A trigger while that run is
// going makes it run once more when it ends; triggered starts are at least
// MinTriggerGap apart (outbox and highlights excepted) and a trigger inside
// the gap waits for it rather than being dropped. A trigger before Run is
// handled once Run starts; one after Run returned does nothing.
func (s *Scheduler) Trigger(name, userID string) error {
	s.mu.Lock()
	j := s.byName[name]
	switch {
	case j == nil:
		s.mu.Unlock()
		return fmt.Errorf("%w %q", ErrUnknownJob, name)
	case j.Scope == Instance:
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrInstanceJob, name)
	case userID == "":
		s.mu.Unlock()
		return ErrNoUser
	}
	j.pending[userID] = struct{}{}
	s.mu.Unlock()
	j.signal()
	return nil
}

// drain starts the triggered runs that are due. A user whose last start of
// this job is within the gap stays pending; drain returns how long until the
// first of those is due, or 0 when none waits.
func (s *Scheduler) drain(ctx context.Context, j *jobState, wg *sync.WaitGroup) time.Duration {
	now := time.Now()
	var ready []string
	var wait time.Duration
	var enabled map[string]bool // nil when the list could not be read: then trust the caller
	if ids, err := s.users(ctx); err == nil {
		enabled = map[string]bool{}
		for _, id := range ids {
			enabled[id] = true
		}
	}
	s.mu.Lock()
	for id := range j.pending {
		if enabled != nil && !enabled[id] { // a disabled user's job never runs, triggered or not
			delete(j.pending, id)
			continue
		}
		if !gapExempt[j.Name] {
			if last, ok := s.lastStart[runKey(j.Name, id)]; ok {
				if d := last.Add(s.gap).Sub(now); d > 0 {
					if wait == 0 || d < wait {
						wait = d
					}
					s.log.Debug("scheduler: trigger waits for the minimum gap", "job", j.Name, "user", id, "wait", d)
					continue
				}
			}
		}
		delete(j.pending, id)
		ready = append(ready, id)
	}
	s.mu.Unlock()
	for _, id := range ready {
		s.start(ctx, j, id, wg, true)
	}
	return wait
}

// start launches one run unless the previous run for the same job and user is
// still going, so a slow run never stacks up behind itself. A triggered start
// that finds a run going sets a flag instead, and the run repeats once when
// it ends.
func (s *Scheduler) start(ctx context.Context, j *jobState, userID string, wg *sync.WaitGroup, triggered bool) {
	key := runKey(j.Name, userID)
	s.mu.Lock()
	if s.running[key] {
		if triggered {
			s.dirty[key] = true
		}
		s.mu.Unlock()
		if triggered {
			s.log.Debug("scheduler: run in progress, will run again when it ends", "job", j.Name, "user", userID)
		} else {
			s.log.Warn("scheduler: previous run still going, skipping", "job", j.Name, "user", userID)
		}
		return
	}
	s.running[key] = true
	s.lastStart[key] = time.Now()
	s.mu.Unlock()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer s.finish(j, userID, key)
		s.runOne(ctx, j.Job, userID)
	}()
}

// finish clears the running mark and queues the one rerun a trigger asked for
// while the run was going.
func (s *Scheduler) finish(j *jobState, userID, key string) {
	s.mu.Lock()
	delete(s.running, key)
	rerun := s.dirty[key]
	delete(s.dirty, key)
	if rerun {
		j.pending[userID] = struct{}{}
	}
	s.mu.Unlock()
	if rerun {
		j.signal()
	}
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
