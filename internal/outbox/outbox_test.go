package outbox

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/store"
)

type fixture struct {
	db  *Runner
	st  *store.Store
	now time.Time
	got []Entry
	err error // what the handler returns
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n'), ('u2','u2','x','n')`); err != nil {
		t.Fatal(err)
	}
	f := &fixture{st: st, now: time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)}
	f.db = &Runner{DB: st.DB(), Now: func() time.Time { return f.now }, MaxAttempts: 3,
		Handle: func(_ context.Context, e Entry) error { f.got = append(f.got, e); return f.err }}
	return f
}

func (f *fixture) enqueue(t *testing.T, user, key string) bool {
	t.Helper()
	ok, err := Enqueue(context.Background(), f.st.DB(), f.now, Entry{UserID: user, Op: "karakeep", Payload: []byte(`{"a":1}`)}, key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func (f *fixture) row(t *testing.T, user string) (status string, attempts int, lastErr string) {
	t.Helper()
	var le *string
	if err := f.st.DB().QueryRow(`SELECT status, attempts, last_error FROM outbox WHERE user_id = ?`, user).Scan(&status, &attempts, &le); err != nil {
		t.Fatal(err)
	}
	if le != nil {
		lastErr = *le
	}
	return
}

func TestEnqueueDedupes(t *testing.T) {
	f := newFixture(t)
	if !f.enqueue(t, "u1", "k1") {
		t.Fatal("first enqueue should insert")
	}
	if f.enqueue(t, "u1", "k1") {
		t.Fatal("same key enqueued twice")
	}
	if !f.enqueue(t, "u2", "k1") {
		t.Fatal("the key is scoped to the user")
	}
}

func TestRunDeliversOnceAndMarksDone(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "u1", "k1")
	st, err := f.db.Run(context.Background(), "u1")
	if err != nil || st.Done != 1 {
		t.Fatalf("stats %+v err %v", st, err)
	}
	if status, _, _ := f.row(t, "u1"); status != "done" {
		t.Fatalf("status %q", status)
	}
	if st, _ = f.db.Run(context.Background(), "u1"); st.Done != 0 || len(f.got) != 1 {
		t.Fatalf("a done entry was sent again: %+v, %d calls", st, len(f.got))
	}
}

func TestFailureBacksOffThenGivesUp(t *testing.T) {
	f := newFixture(t)
	f.err = errors.New("boom")
	f.enqueue(t, "u1", "k1")
	ctx := context.Background()

	st, _ := f.db.Run(ctx, "u1")
	if st.Retried != 1 {
		t.Fatalf("stats %+v", st)
	}
	status, attempts, lastErr := f.row(t, "u1")
	if status != "pending" || attempts != 1 || lastErr != "boom" {
		t.Fatalf("got %s %d %q", status, attempts, lastErr)
	}
	// Not due yet: nothing is sent until the backoff has passed.
	if st, _ = f.db.Run(ctx, "u1"); st.Retried != 0 || len(f.got) != 1 {
		t.Fatalf("retried too early: %+v", st)
	}
	f.now = f.now.Add(Backoff(1) + time.Second)
	f.db.Run(ctx, "u1")
	f.now = f.now.Add(Backoff(2) + time.Second)
	st, _ = f.db.Run(ctx, "u1")
	if st.Failed != 1 {
		t.Fatalf("expected give up after MaxAttempts: %+v", st)
	}
	if status, attempts, _ := f.row(t, "u1"); status != "failed" || attempts != 3 {
		t.Fatalf("got %s %d", status, attempts)
	}
}

func TestPermanentErrorFailsAtOnce(t *testing.T) {
	f := newFixture(t)
	f.err = permanentErr{errors.New("401")}
	f.enqueue(t, "u1", "k1")
	if st, _ := f.db.Run(context.Background(), "u1"); st.Failed != 1 {
		t.Fatalf("stats %+v", st)
	}
	if status, attempts, _ := f.row(t, "u1"); status != "failed" || attempts != 1 {
		t.Fatalf("got %s %d", status, attempts)
	}
}

type permanentErr struct{ error }

func (permanentErr) Permanent() bool { return true }

func TestRunIsScopedToTheUser(t *testing.T) {
	f := newFixture(t)
	f.enqueue(t, "u1", "k1")
	f.enqueue(t, "u2", "k1")
	f.db.Run(context.Background(), "u1")
	if len(f.got) != 1 || f.got[0].UserID != "u1" {
		t.Fatalf("got %+v", f.got)
	}
	if status, _, _ := f.row(t, "u2"); status != "pending" {
		t.Fatalf("u2's entry was touched: %s", status)
	}
}

func TestEntriesRunInOrder(t *testing.T) {
	f := newFixture(t)
	for _, k := range []string{"a", "b", "c"} {
		f.enqueue(t, "u1", k)
		f.now = f.now.Add(time.Second)
	}
	f.db.Run(context.Background(), "u1")
	for i, k := range []string{"a", "b", "c"} {
		if f.got[i].Key != k {
			t.Fatalf("order: %+v", f.got)
		}
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	if Backoff(1) >= Backoff(2) || Backoff(2) >= Backoff(3) {
		t.Fatal("backoff should grow")
	}
	if Backoff(50) != MaxBackoff {
		t.Fatalf("backoff not capped: %v", Backoff(50))
	}
}
