package writeback

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/dest"
	"github.com/Jolls/Siftstr/internal/outbox"
)

// colorEntry queues the highlight color change a user's click makes: no action,
// no item, and the color it was queued with in the payload.
func (f *fixture) colorEntry(user, color string) {
	f.t.Helper()
	var conn string
	if err := f.st.DB().QueryRow(`SELECT id FROM connections WHERE user_id = ? AND kind = 'karakeep'`, user).Scan(&conn); err != nil {
		f.t.Fatal(err)
	}
	payload, _ := json.Marshal(dest.Item{ID: "h1", Verb: dest.VerbHighlightColor, HighlightID: "h1", HighlightColor: color})
	if _, err := outbox.Enqueue(context.Background(), f.st.DB(), f.now, outbox.Entry{UserID: user, ConnectionID: conn, Op: Karakeep, Payload: payload}, ""); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) colorCalls() (out []dest.Item) {
	for _, c := range f.calls {
		if c.item.Verb == dest.VerbHighlightColor {
			out = append(out, c.item)
		}
	}
	return
}

func (f *fixture) outboxStatus() (status string, attempts int) {
	f.t.Helper()
	_ = f.st.DB().QueryRow(`SELECT status, attempts FROM outbox`).Scan(&status, &attempts)
	return
}

func TestHighlightColorEntryIsSentWithTheCurrentStoredColor(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.connect("u2", Karakeep)
	f.exec(`INSERT INTO highlight_picks (user_id, day, highlight_id, text, color, created_at) VALUES
		('u1', '2026-10-09', 'h1', 'one', 'blue', 'c'), ('u2', '2026-10-09', 'h1', 'two', 'green', 'c')`)
	f.colorEntry("u1", "red") // queued with red, but u1's pick has since become blue
	f.drain()
	calls := f.colorCalls()
	if len(calls) != 1 || calls[0].HighlightID != "h1" || calls[0].HighlightColor != "blue" {
		t.Fatalf("calls = %+v, want one with u1's current color blue", calls)
	}
	if st, _ := f.outboxStatus(); st != "done" {
		t.Errorf("status = %s", st)
	}
}

func TestHighlightColorEntryWithNoPickIsDone(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.colorEntry("u1", "red")
	f.drain()
	if n := len(f.colorCalls()); n != 0 {
		t.Errorf("%d calls for a pick that is gone", n)
	}
	if st, _ := f.outboxStatus(); st != "done" {
		t.Errorf("status = %s, want done", st)
	}
}

func TestHighlightColorEntryRetriesThenSucceeds(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.exec(`INSERT INTO highlight_picks (user_id, day, highlight_id, text, color, created_at) VALUES ('u1', '2026-10-09', 'h1', 'one', 'red', 'c')`)
	f.colorEntry("u1", "red")
	f.fail[Karakeep] = errors.New("down")
	f.drain()
	if st, n := f.outboxStatus(); st != "pending" || n != 1 {
		t.Fatalf("after a failure: %s, %d attempts", st, n)
	}
	delete(f.fail, Karakeep)
	f.now = f.now.Add(outbox.Backoff(1) + time.Second)
	f.drain()
	if st, _ := f.outboxStatus(); st != "done" || len(f.colorCalls()) != 1 {
		t.Errorf("after the retry: %s, %d calls", st, len(f.colorCalls()))
	}
}
