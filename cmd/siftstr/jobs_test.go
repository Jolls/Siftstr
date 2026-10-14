package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/dest"
	"github.com/Jolls/Siftstr/internal/scheduler"
	"github.com/Jolls/Siftstr/internal/store"
	"github.com/Jolls/Siftstr/internal/triage"
)

// A job name the server triggers must be registered, or the trigger is a
// silent no-op.
func TestEveryJobNameIsRegistered(t *testing.T) {
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	conns, err := connections.New(st.DB(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	sched := scheduler.New(func(context.Context) ([]string, error) { return nil, nil }, nil)
	if err := registerJobs(sched, st, conns, triage.New(st.DB()), map[string]dest.Factory{}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, name := range scheduler.JobNames {
		err := sched.Trigger(name, "u1")
		if errors.Is(err, scheduler.ErrUnknownJob) {
			t.Errorf("job %q is not registered", name)
		}
	}
}
