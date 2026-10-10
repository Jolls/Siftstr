package outbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func (f *fixture) log() *Log { return &Log{DB: f.st.DB()} }

func (f *fixture) statuses(t *testing.T, user string) []string {
	t.Helper()
	rows, err := f.log().Recent(context.Background(), user, 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		out = append(out, r.Status)
	}
	return out
}

func TestActivityStatusesTellPausedFromFailed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.enqueue(t, "u1", "queued")
	f.enqueue(t, "u1", "paused")
	f.enqueue(t, "u1", "failed")
	f.enqueue(t, "u1", "done")
	// Drive each into its state by hand-run handlers.
	byKey := map[string]error{"paused": errors.New("down"), "failed": permanentErr{errors.New("bad")}, "done": nil}
	f.db.Handle = func(_ context.Context, e Entry) error {
		if e.Key == "queued" {
			return errors.New("later") // retrying
		}
		return byKey[e.Key]
	}
	f.db.MaxAttempts = 1
	f.db.Run(ctx, "u1")
	// "queued" hit MaxAttempts too; put it back to a fresh queued entry.
	f.exec(t, `UPDATE outbox SET status = 'pending', attempts = 0, permanent = 0 WHERE dedupe_key = 'queued'`)

	got := map[string]bool{}
	for _, s := range f.statuses(t, "u1") {
		got[s] = true
	}
	for _, want := range []string{StatusQueued, StatusPaused, StatusFailed, StatusDone} {
		if !got[want] {
			t.Errorf("no %q entry in %v", want, got)
		}
	}
	if n, _ := f.log().Problems(ctx, "u1"); n != 2 {
		t.Errorf("problems = %d, want 2 (paused and failed)", n)
	}
}

func TestRetryRequeuesWithFreshBudget(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.err = errors.New("down")
	f.enqueue(t, "u1", "k1")
	f.db.MaxAttempts = 1
	f.db.Run(ctx, "u1")
	rows, _ := f.log().Recent(ctx, "u1", 10)
	if len(rows) != 1 || rows[0].Status != StatusPaused {
		t.Fatalf("rows = %+v", rows)
	}
	if err := f.log().Retry(ctx, "u1", rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if status, attempts, _ := f.row(t, "u1"); status != "pending" || attempts != 0 {
		t.Fatalf("after retry: %s %d", status, attempts)
	}
	f.err = nil
	f.db.Run(ctx, "u1")
	if status, _, _ := f.row(t, "u1"); status != "done" {
		t.Fatalf("retried entry ended %s", status)
	}
	if n, _ := f.log().Problems(ctx, "u1"); n != 0 {
		t.Errorf("problems = %d after a successful retry", n)
	}
	// A finished entry cannot be retried again.
	if err := f.log().Retry(ctx, "u1", rows[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("retrying a done entry = %v", err)
	}
}

func TestDismissHidesWithoutSending(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.err = permanentErr{errors.New("bad")}
	f.enqueue(t, "u1", "k1")
	f.db.Run(ctx, "u1")
	rows, _ := f.log().Recent(ctx, "u1", 10)
	if err := f.log().Dismiss(ctx, "u1", rows[0].ID, f.now); err != nil {
		t.Fatal(err)
	}
	if rows, _ = f.log().Recent(ctx, "u1", 10); len(rows) != 0 {
		t.Errorf("dismissed entry still listed: %+v", rows)
	}
	if n, _ := f.log().Problems(ctx, "u1"); n != 0 {
		t.Errorf("problems = %d after dismiss", n)
	}
	if len(f.got) != 1 {
		t.Errorf("dismiss sent something: %d calls", len(f.got))
	}
}

func TestActivityIsScopedToTheUser(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.err = permanentErr{errors.New("bad")}
	f.enqueue(t, "u1", "k1")
	f.db.Run(ctx, "u1")
	rows, _ := f.log().Recent(ctx, "u1", 10)
	id := rows[0].ID

	if other, _ := f.log().Recent(ctx, "u2", 10); len(other) != 0 {
		t.Errorf("u2 sees u1's entries: %+v", other)
	}
	if n, _ := f.log().Problems(ctx, "u2"); n != 0 {
		t.Errorf("u2 alert counts u1's problem: %d", n)
	}
	if err := f.log().Retry(ctx, "u2", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("u2 retried u1's entry: %v", err)
	}
	if err := f.log().Dismiss(ctx, "u2", id, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("u2 dismissed u1's entry: %v", err)
	}
	if n, _ := f.log().Problems(ctx, "u1"); n != 1 {
		t.Errorf("u1's problem was changed by u2: %d", n)
	}
}

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.DB().Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
