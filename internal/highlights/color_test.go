package highlights

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/dest"
)

const colorDay = "2026-10-11"

// colorFixture has a Karakeep connection and a pick for each of u1 and u2. Both
// picks have the highlight ID h1 on purpose: nothing may mix them up.
func colorFixture(t *testing.T) (*fixture, *Colors) {
	t.Helper()
	f := newFixture(t)
	f.connect("u1")
	f.connect("u2")
	f.exec(`INSERT INTO highlight_picks (user_id, day, highlight_id, text, color, created_at) VALUES
		('u1', ?, 'h1', 'one', 'yellow', 'c'), ('u2', ?, 'h1', 'two', 'blue', 'c')`, colorDay, colorDay)
	return f, &Colors{DB: f.st.DB(), Now: func() time.Time { return f.now }}
}

func (f *fixture) pickColor(user string) (color string) {
	f.t.Helper()
	if err := f.st.DB().QueryRow(`SELECT color FROM highlight_picks WHERE user_id = ? AND day = ? AND highlight_id = 'h1'`, user, colorDay).Scan(&color); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *fixture) outboxRows(user string) int {
	return f.count(`SELECT COUNT(*) FROM outbox WHERE user_id = ?`, user)
}

func TestSetColorUpdatesThePickAndQueuesOneEntry(t *testing.T) {
	f, c := colorFixture(t)
	res, err := c.SetColor(context.Background(), "u1", colorDay, "h1", "red")
	if err != nil || res != (ColorResult{Changed: true, Queued: true}) {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if got, other := f.pickColor("u1"), f.pickColor("u2"); got != "red" || other != "blue" {
		t.Errorf("colors = %q, %q; want red for u1 and blue left alone for u2", got, other)
	}
	if n := f.outboxRows("u1"); n != 1 {
		t.Fatalf("u1 has %d outbox rows", n)
	}
	if n := f.outboxRows("u2"); n != 0 {
		t.Errorf("u2 has %d outbox rows", n)
	}
	var op, status, payload string
	var conn, action, key *string
	if err := f.st.DB().QueryRow(`SELECT op, status, payload, connection_id, action_id, dedupe_key FROM outbox WHERE user_id = 'u1'`).
		Scan(&op, &status, &payload, &conn, &action, &key); err != nil {
		t.Fatal(err)
	}
	if op != "karakeep" || status != "pending" || action != nil || key != nil || conn == nil {
		t.Errorf("row = %s %s conn=%v action=%v key=%v", op, status, conn, action, key)
	}
	var wantConn string
	_ = f.st.DB().QueryRow(`SELECT id FROM connections WHERE user_id = 'u1' AND kind = 'karakeep'`).Scan(&wantConn)
	if conn != nil && *conn != wantConn {
		t.Errorf("connection = %q, want u1's %q", *conn, wantConn)
	}
	var it dest.Item
	if err := json.Unmarshal([]byte(payload), &it); err != nil {
		t.Fatal(err)
	}
	if it.Verb != dest.VerbHighlightColor || it.HighlightID != "h1" || it.HighlightColor != "red" || it.Title != "Highlight color: Red" {
		t.Errorf("payload = %+v", it)
	}
}

func TestSetColorToTheSameColorDoesNothing(t *testing.T) {
	f, c := colorFixture(t)
	res, err := c.SetColor(context.Background(), "u1", colorDay, "h1", "yellow")
	if err != nil || res != (ColorResult{}) || f.outboxRows("u1") != 0 {
		t.Errorf("res = %+v, %v, rows = %d", res, err, f.outboxRows("u1"))
	}
}

func TestChangingBackQueuesAnotherEntry(t *testing.T) {
	f, c := colorFixture(t)
	for _, col := range []string{"red", "yellow"} {
		if _, err := c.SetColor(context.Background(), "u1", colorDay, "h1", col); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.outboxRows("u1"); n != 2 || f.pickColor("u1") != "yellow" {
		t.Errorf("rows = %d, color = %q; want 2 and yellow", n, f.pickColor("u1"))
	}
}

func TestSetColorRejectsAColorKarakeepDoesNotHave(t *testing.T) {
	f, c := colorFixture(t)
	for _, col := range []string{"purple", "", "Red"} {
		if _, err := c.SetColor(context.Background(), "u1", colorDay, "h1", col); !errors.Is(err, ErrBadColor) {
			t.Errorf("%q: err = %v, want ErrBadColor", col, err)
		}
	}
	if f.pickColor("u1") != "yellow" || f.outboxRows("u1") != 0 {
		t.Error("a rejected color changed something")
	}
}

func TestSetColorWithoutAConnectionStillUpdates(t *testing.T) {
	f, c := colorFixture(t)
	f.exec(`DELETE FROM connections WHERE user_id = 'u1'`)
	res, err := c.SetColor(context.Background(), "u1", colorDay, "h1", "green")
	if err != nil || res != (ColorResult{Changed: true}) {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if f.pickColor("u1") != "green" || f.outboxRows("u1") != 0 {
		t.Error("want the stored color changed and nothing queued")
	}
}

func TestSetColorIsScopedToTheUser(t *testing.T) {
	f, c := colorFixture(t)
	// Both users have a pick with the ID h1: each post changes only the poster's.
	if _, err := c.SetColor(context.Background(), "u2", colorDay, "h1", "green"); err != nil {
		t.Fatal(err)
	}
	if f.pickColor("u2") != "green" || f.pickColor("u1") != "yellow" {
		t.Errorf("u2 posting left u2 at %q and u1 at %q", f.pickColor("u2"), f.pickColor("u1"))
	}
	if _, err := c.SetColor(context.Background(), "u1", colorDay, "h1", "red"); err != nil || f.pickColor("u2") != "green" || f.pickColor("u1") != "red" {
		t.Errorf("u1 posting: err = %v, u1 = %q, u2 = %q", err, f.pickColor("u1"), f.pickColor("u2"))
	}

	// An ID only u1 has is not reachable by u2.
	f.exec(`UPDATE highlight_picks SET highlight_id = 'only_u1' WHERE user_id = 'u1'`)
	before := f.outboxRows("u2")
	if _, err := c.SetColor(context.Background(), "u2", colorDay, "only_u1", "blue"); !errors.Is(err, ErrNoPick) {
		t.Errorf("u2 reaching u1's pick: err = %v, want ErrNoPick", err)
	}
	var got string
	_ = f.st.DB().QueryRow(`SELECT color FROM highlight_picks WHERE user_id = 'u1'`).Scan(&got)
	if got != "red" || f.outboxRows("u2") != before {
		t.Errorf("u1's pick = %q, u2 rows %d -> %d; want it untouched and nothing queued", got, before, f.outboxRows("u2"))
	}
}

func TestSetColorNeedsTheRightDayAndHighlight(t *testing.T) {
	f, c := colorFixture(t)
	if _, err := c.SetColor(context.Background(), "u1", "2026-10-10", "h1", "red"); !errors.Is(err, ErrNoPick) {
		t.Errorf("wrong day: err = %v", err)
	}
	if _, err := c.SetColor(context.Background(), "u1", colorDay, "nope", "red"); !errors.Is(err, ErrNoPick) {
		t.Errorf("wrong highlight: err = %v", err)
	}
	if f.pickColor("u1") != "yellow" || f.outboxRows("u1") != 0 {
		t.Error("a failed change touched something")
	}
}
