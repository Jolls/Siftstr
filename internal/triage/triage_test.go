package triage

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/store"
)

type fixture struct {
	s   *Service
	st  *store.Store
	now time.Time
}

// newFixture has two users. u1: src1 (deep), src2 (light only), src3 (digest).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{st: st, now: time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)}
	f.s = &Service{DB: st.DB(), Now: func() time.Time { return f.now }}
	f.exec(t, `INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n'), ('u2','u2','x','n')`)
	f.exec(t, `INSERT INTO sources (id, user_id, kind, external_id, name, granularity, max_depth) VALUES
		('src1','u1','miniflux_feed','1','Blog','individual','deep'),
		('src2','u1','miniflux_feed','2','Short','individual','light'),
		('src3','u1','miniflux_feed','3','News','digest','light'),
		('src9','u2','miniflux_feed','1','Other','individual','deep')`)
	f.exec(t, `INSERT INTO digests (id, user_id, source_id, batch_date, summary, state) VALUES ('dig1','u1','src3','2026-10-09','s','light')`)
	return f
}

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.DB().Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) item(t *testing.T, id, user, src, state string, digest string) {
	t.Helper()
	var d any
	if digest != "" {
		d = digest
	}
	f.exec(t, `INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, digest_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'article', 'https://example.com/x', 't', ?, ?, 'c', 'c')`, id, user, src, id, state, d)
}

func (f *fixture) state(t *testing.T, id string) (state string, promoted bool) {
	t.Helper()
	var p *string
	if err := f.st.DB().QueryRow(`SELECT state, promoted_at FROM items WHERE id = ?`, id).Scan(&state, &p); err != nil {
		t.Fatal(err)
	}
	return state, p != nil
}

func (f *fixture) apply(t *testing.T, user string, in ...Action) []Result {
	t.Helper()
	out, err := f.s.Apply(context.Background(), user, in)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *fixture) at(d time.Duration) time.Time { return f.now.Add(d) }

func act(id, subj, kind string, at time.Time) Action {
	return Action{ActionID: id, SubjectID: subj, Kind: kind, At: at}
}

func want(t *testing.T, r Result, status, state string) {
	t.Helper()
	if r.Status != status || r.State != state {
		t.Fatalf("got %s/%s (%s), want %s/%s", r.Status, r.State, r.Reason, status, state)
	}
}

func TestTransitions(t *testing.T) {
	cases := []struct {
		name, from, kind, to string
	}{
		{"archive light", "light", Archive, "archived"},
		{"keep light", "light", Keep, "kept"},
		{"promote light", "light", Promote, "pending_deep"},
		{"archive deep", "deep", Archive, "archived"},
		{"keep deep", "deep", Keep, "kept"},
		{"promote deep acts as keep", "deep", Promote, "kept"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.item(t, "i1", "u1", "src1", c.from, "")
			want(t, f.apply(t, "u1", act("a1", "i1", c.kind, f.now))[0], Applied, c.to)
			if got, _ := f.state(t, "i1"); got != c.to {
				t.Fatalf("db state %s, want %s", got, c.to)
			}
		})
	}
	for _, from := range []string{"pending_light", "pending_deep", "expired"} {
		for _, kind := range []string{Archive, Promote, Keep} {
			f := newFixture(t)
			f.item(t, "i1", "u1", "src1", from, "")
			r := f.apply(t, "u1", act("a1", "i1", kind, f.now))[0]
			if r.Status != Rejected || r.State != from {
				t.Errorf("%s from %s: got %s/%s", kind, from, r.Status, r.State)
			}
		}
	}
}

func TestPromoteIsDisabledForLightOnlySourcesAndDigests(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src2", "light", "")
	r := f.apply(t, "u1", act("a1", "i1", Promote, f.now))[0]
	if r.Status != Rejected || r.Reason != ReasonPromoteOff || r.State != "light" {
		t.Fatalf("light-only promote: %+v", r)
	}
	// keep still works on a light-only item
	want(t, f.apply(t, "u1", act("a2", "i1", Keep, f.now))[0], Applied, "kept")

	d := f.apply(t, "u1", Action{ActionID: "a3", SubjectType: SubjectDigest, SubjectID: "dig1", Kind: Promote, At: f.now})[0]
	if d.Status != Rejected || d.Reason != ReasonPromoteOff {
		t.Fatalf("digest promote: %+v", d)
	}
}

func TestPromoteOnce(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	want(t, f.apply(t, "u1", act("a1", "i1", Promote, f.now))[0], Applied, "pending_deep")
	// undo spends nothing: promote is allowed again
	want(t, f.apply(t, "u1", Action{ActionID: "u1", SubjectID: "i1", Kind: Undo, TargetActionID: "a1", At: f.at(time.Second)})[0], Applied, "light")
	if _, promoted := f.state(t, "i1"); promoted {
		t.Fatal("undo should clear promoted_at")
	}
	want(t, f.apply(t, "u1", act("a2", "i1", Promote, f.at(2*time.Second)))[0], Applied, "pending_deep")

	// An item that was promoted and came back deep, then carried to light by
	// an operator fix, still cannot be promoted twice.
	f.exec(t, `UPDATE actions SET status = 'released'`)
	f.exec(t, `UPDATE items SET state = 'light' WHERE id = 'i1'`)
	r := f.apply(t, "u1", act("a3", "i1", Promote, f.at(time.Minute)))[0]
	if r.Status != Rejected || r.Reason != ReasonPromoteOff {
		t.Fatalf("second promote: %+v", r)
	}
}

func TestDuplicateBatchIsHarmless(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	batch := []Action{act("a1", "i1", Archive, f.now), act("a2", "missing", Archive, f.now)}
	first := f.apply(t, "u1", batch...)
	want(t, first[0], Applied, "archived")
	if first[1].Reason != ReasonUnknownSubject {
		t.Fatalf("got %+v", first[1])
	}
	f.now = f.now.Add(time.Minute)
	again := f.apply(t, "u1", batch...)
	want(t, again[0], Duplicate, "archived")
	if again[1].Status != Rejected || again[1].Reason != ReasonUnknownSubject {
		t.Fatalf("replayed rejection: %+v", again[1])
	}
	var n int
	f.st.DB().QueryRow(`SELECT COUNT(*) FROM actions`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d action rows, want 2", n)
	}
	// The hold did not restart.
	var release string
	f.st.DB().QueryRow(`SELECT release_at FROM actions WHERE action_id = 'a1'`).Scan(&release)
	if release != stamp(f.now.Add(-time.Minute+DefaultHold)) {
		t.Fatalf("release_at moved: %s", release)
	}
}

func TestUndoWithinAndAfterHold(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.item(t, "i2", "u1", "src1", "light", "")
	f.apply(t, "u1", act("a1", "i1", Keep, f.now), act("a2", "i2", Keep, f.now))

	f.now = f.now.Add(DefaultHold - time.Second)
	want(t, f.apply(t, "u1", Action{ActionID: "u1", SubjectID: "i1", Kind: Undo, TargetActionID: "a1", At: f.now})[0], Applied, "light")
	var status string
	f.st.DB().QueryRow(`SELECT status FROM actions WHERE action_id = 'a1'`).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("a1 is %s", status)
	}

	// Undoing twice is refused, not a second revert.
	r := f.apply(t, "u1", Action{ActionID: "u1b", SubjectID: "i1", Kind: Undo, TargetActionID: "a1", At: f.now})[0]
	if r.Status != Rejected || r.Reason != ReasonNothingToUndo {
		t.Fatalf("double undo: %+v", r)
	}

	// After the hold, even before the timer has run, undo is refused.
	f.now = f.now.Add(2 * time.Second)
	r = f.apply(t, "u1", Action{ActionID: "u2", SubjectID: "i2", Kind: Undo, TargetActionID: "a2", At: f.now})[0]
	if r.Status != Rejected || r.Reason != ReasonHoldEnded || r.State != "kept" {
		t.Fatalf("late undo: %+v", r)
	}
}

func TestUndoQueuedWithItsActionArrivesTogether(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	// Sent in reverse order, as a reconnecting queue might.
	out := f.apply(t, "u1",
		Action{ActionID: "u1", SubjectID: "i1", Kind: Undo, TargetActionID: "a1", At: f.at(2 * time.Second)},
		act("a1", "i1", Archive, f.at(time.Second)))
	want(t, out[0], Applied, "light")
	want(t, out[1], Applied, "archived")
	if s, _ := f.state(t, "i1"); s != "light" {
		t.Fatalf("final state %s", s)
	}
}

func TestLastWriteWins(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	want(t, f.apply(t, "u1", act("phone", "i1", Archive, f.at(1*time.Second)))[0], Applied, "archived")
	// Later action from another device replaces it.
	want(t, f.apply(t, "u1", act("desk", "i1", Keep, f.at(5*time.Second)))[0], Applied, "kept")
	var status string
	f.st.DB().QueryRow(`SELECT status FROM actions WHERE action_id = 'phone'`).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("replaced action is %s", status)
	}
	// An older one arriving late loses.
	r := f.apply(t, "u1", act("late", "i1", Archive, f.at(3*time.Second)))[0]
	if r.Status != Rejected || r.Reason != ReasonOlder || r.State != "kept" {
		t.Fatalf("older action: %+v", r)
	}
	// The replaced action cannot be undone, only the current one.
	r = f.apply(t, "u1", Action{ActionID: "u1", SubjectID: "i1", Kind: Undo, TargetActionID: "phone", At: f.at(6 * time.Second)})[0]
	if r.Status != Rejected {
		t.Fatalf("undo of replaced: %+v", r)
	}
	want(t, f.apply(t, "u1", Action{ActionID: "u2", SubjectID: "i1", Kind: Undo, TargetActionID: "desk", At: f.at(7 * time.Second)})[0], Applied, "light")
}

func TestLastWriteWinsAppliesRulesToTheRestoredState(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.apply(t, "u1", act("a1", "i1", Promote, f.at(time.Second)))
	// Archive replaces the held promote and clears promoted_at.
	want(t, f.apply(t, "u1", act("a2", "i1", Archive, f.at(2*time.Second)))[0], Applied, "archived")
	if _, promoted := f.state(t, "i1"); promoted {
		t.Fatal("promoted_at should be cleared when a promote is replaced")
	}
}

func TestReleasedActionCannotBeReplaced(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.apply(t, "u1", act("a1", "i1", Archive, f.now))
	f.now = f.now.Add(DefaultHold + time.Second)
	due, err := f.s.ReleaseDue(context.Background(), "u1")
	if err != nil || len(due) != 1 || due[0] != "a1" {
		t.Fatalf("due %v, %v", due, err)
	}
	if again, _ := f.s.ReleaseDue(context.Background(), "u1"); len(again) != 0 {
		t.Fatalf("released twice: %v", again)
	}
	r := f.apply(t, "u1", act("a2", "i1", Keep, f.now))[0]
	if r.Status != Rejected || r.Reason != ReasonSent || r.State != "archived" {
		t.Fatalf("replace after release: %+v", r)
	}
}

func TestHeldActionsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	st, err := store.OpenFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	st.DB().Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n')`)
	st.DB().Exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('src1','u1','miniflux_feed','1','B')`)
	st.DB().Exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		VALUES ('i1','u1','src1','e','article','u','t','light','c','c')`)
	s := &Service{DB: st.DB(), Now: func() time.Time { return now }}
	if _, err := s.Apply(ctx, "u1", []Action{act("a1", "i1", Keep, now)}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st, err = store.OpenFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	later := now.Add(5 * time.Minute)
	s = &Service{DB: st.DB(), Now: func() time.Time { return later }}
	out, err := s.Apply(ctx, "u1", []Action{{ActionID: "u1", SubjectID: "i1", Kind: Undo, TargetActionID: "a1", At: later}})
	if err != nil {
		t.Fatal(err)
	}
	want(t, out[0], Applied, "light")
}

func TestDigestActionsCascadeToChildren(t *testing.T) {
	f := newFixture(t)
	f.item(t, "c1", "u1", "src3", "light", "dig1")
	f.item(t, "c2", "u1", "src3", "light", "dig1")
	f.item(t, "c3", "u1", "src3", "pending_light", "dig1") // not covered by the summary
	d := func(id, kind string, at time.Time) Action {
		return Action{ActionID: id, SubjectType: SubjectDigest, SubjectID: "dig1", Kind: kind, At: at}
	}
	want(t, f.apply(t, "u1", d("a1", Archive, f.now))[0], Applied, "archived")
	for _, c := range []string{"c1", "c2"} {
		if s, _ := f.state(t, c); s != "archived" {
			t.Fatalf("%s is %s", c, s)
		}
	}
	if s, _ := f.state(t, "c3"); s != "pending_light" {
		t.Fatalf("c3 is %s", s)
	}
	want(t, f.apply(t, "u1", Action{ActionID: "u1", SubjectType: SubjectDigest, SubjectID: "dig1", Kind: Undo, TargetActionID: "a1", At: f.at(time.Second)})[0], Applied, "light")
	if s, _ := f.state(t, "c1"); s != "light" {
		t.Fatalf("c1 is %s after undo", s)
	}
	want(t, f.apply(t, "u1", d("a2", Keep, f.at(2*time.Second)))[0], Applied, "kept")
	if s, _ := f.state(t, "c2"); s != "kept" {
		t.Fatalf("c2 is %s", s)
	}
}

func TestIsolationBetweenUsers(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.item(t, "i9", "u2", "src9", "light", "")
	f.apply(t, "u1", act("a1", "i1", Archive, f.now))

	// u2 cannot act on u1's item, undo u1's action, or reuse u1's action_id.
	r := f.apply(t, "u2", act("b1", "i1", Archive, f.now))[0]
	if r.Status != Rejected || r.Reason != ReasonUnknownSubject || r.State != "" {
		t.Fatalf("cross-user act: %+v", r)
	}
	r = f.apply(t, "u2", Action{ActionID: "b2", SubjectID: "i9", Kind: Undo, TargetActionID: "a1", At: f.now})[0]
	if r.Status != Rejected || r.Reason != ReasonNothingToUndo {
		t.Fatalf("cross-user undo: %+v", r)
	}
	r = f.apply(t, "u2", act("a1", "i9", Archive, f.now))[0]
	if r.Status != Rejected || r.Reason != ReasonIDInUse || r.State != "" {
		t.Fatalf("reused id: %+v", r)
	}
	if s, _ := f.state(t, "i1"); s != "archived" {
		t.Fatalf("u1's item is %s", s)
	}
	if s, _ := f.state(t, "i9"); s != "light" {
		t.Fatalf("u2's item is %s", s)
	}
}

func TestClientClockCannotWinByBeingAhead(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.apply(t, "u1", act("a1", "i1", Archive, f.at(24*time.Hour))) // clock a day ahead
	r := f.apply(t, "u1", act("a2", "i1", Keep, f.at(time.Minute)))[0]
	want(t, r, Applied, "kept")
}

func TestInvalidActions(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	for _, a := range []Action{
		{ActionID: "", SubjectID: "i1", Kind: Archive, At: f.now},
		{ActionID: "x", SubjectID: "i1", Kind: "delete", At: f.now},
		{ActionID: "y", SubjectID: "i1", Kind: Undo, At: f.now}, // no target
	} {
		if r := f.apply(t, "u1", a)[0]; r.Status != Rejected {
			t.Errorf("%+v accepted", a)
		}
	}
	if s, _ := f.state(t, "i1"); s != "light" {
		t.Fatal("state changed")
	}
}

func TestUserHoldSetting(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.exec(t, `INSERT INTO user_settings (user_id, key, value) VALUES ('u1','undo_hold_minutes','1')`)
	f.apply(t, "u1", act("a1", "i1", Keep, f.now))
	f.now = f.now.Add(2 * time.Minute)
	r := f.apply(t, "u1", Action{ActionID: "u1", SubjectID: "i1", Kind: Undo, TargetActionID: "a1", At: f.now})[0]
	if r.Reason != ReasonHoldEnded {
		t.Fatalf("got %+v", r)
	}
}

func TestUnknownSubjectTypeIsRejectedNotFatal(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	out := f.apply(t, "u1",
		Action{ActionID: "bad", SubjectType: "foo", SubjectID: "i1", Kind: Archive, At: f.now},
		act("ok", "i1", Keep, f.at(time.Second)))
	if out[0].Status != Rejected || out[0].Reason != ReasonBadAction {
		t.Fatalf("bad type: %+v", out[0])
	}
	want(t, out[1], Applied, "kept") // the rest of the batch is not lost
}

func TestMissingOrFutureTimestampSortsAsNow(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	// The undo has no timestamp and its action is a day ahead; both mean "now",
	// so they apply in the order given rather than undo-first.
	out := f.apply(t, "u1",
		act("a1", "i1", Archive, f.at(24*time.Hour)),
		Action{ActionID: "u1", SubjectID: "i1", Kind: Undo, TargetActionID: "a1"})
	want(t, out[0], Applied, "archived")
	want(t, out[1], Applied, "light")
}

func TestExpiredHoldCountsAsSentBeforeTheTimerRuns(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.apply(t, "u1", act("a1", "i1", Archive, f.now))
	f.now = f.now.Add(DefaultHold + time.Minute) // ReleaseDue has not run
	r := f.apply(t, "u1", act("a2", "i1", Keep, f.now))[0]
	if r.Status != Rejected || r.Reason != ReasonSent || r.State != "archived" {
		t.Fatalf("got %+v", r)
	}
}

func TestStateChangeDuringHoldBlocksUndoAndReplace(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.apply(t, "u1", act("a1", "i1", Promote, f.now))
	// The morning run delivers the deep summary while the hold is still open.
	f.exec(t, `UPDATE items SET state = 'deep' WHERE id = 'i1'`)
	r := f.apply(t, "u1", Action{ActionID: "u1", SubjectID: "i1", Kind: Undo, TargetActionID: "a1", At: f.at(time.Second)})[0]
	if r.Status != Rejected || r.State != "deep" {
		t.Fatalf("undo after summary: %+v", r)
	}
	r = f.apply(t, "u1", act("a2", "i1", Archive, f.at(2*time.Second)))[0]
	if r.Status != Rejected || r.Reason != ReasonSent || r.State != "deep" {
		t.Fatalf("replace after summary: %+v", r)
	}
	if _, promoted := f.state(t, "i1"); !promoted {
		t.Fatal("promoted_at was cleared")
	}
}

func TestReleaseDueIsScopedToTheUser(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.item(t, "i9", "u2", "src9", "light", "")
	f.apply(t, "u1", act("a1", "i1", Keep, f.now))
	f.apply(t, "u2", act("b1", "i9", Keep, f.now))
	f.now = f.now.Add(DefaultHold + time.Second)
	due, err := f.s.ReleaseDue(context.Background(), "u1")
	if err != nil || len(due) != 1 || due[0] != "a1" {
		t.Fatalf("u1 due %v, %v", due, err)
	}
	var status string
	f.st.DB().QueryRow(`SELECT status FROM actions WHERE action_id = 'b1'`).Scan(&status)
	if status != "held" {
		t.Fatalf("u2's action is %s", status)
	}
}

func (f *fixture) cardIDs(t *testing.T, user, list string) map[string]Card {
	t.Helper()
	cards, err := f.s.Cards(context.Background(), user, list, "2026-10-09")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]Card{}
	for _, c := range cards {
		m[c.ID] = c
	}
	return m
}

func TestCardsTodayAndBacklog(t *testing.T) {
	f := newFixture(t)
	mk := func(id, src, state, day string) {
		f.item(t, id, "u1", src, state, "")
		f.exec(t, `UPDATE items SET batch_date = ?, light_summary = 'light', title = ? WHERE id = ?`, day, id, id)
	}
	mk("today_light", "src1", "light", "2026-10-09")
	mk("today_light_only", "src2", "light", "2026-10-09")
	mk("today_deep", "src1", "deep", "2026-10-09")
	mk("old_light", "src1", "light", "2026-10-08")
	mk("old_pending", "src1", "pending_light", "2026-10-08")
	mk("today_waiting", "src1", "pending_light", "2026-10-09")
	mk("today_expired", "src1", "expired", "2026-10-09")
	f.item(t, "child", "u1", "src3", "light", "dig1")
	f.exec(t, `UPDATE items SET batch_date = '2026-10-09' WHERE id = 'child'`)
	f.exec(t, `UPDATE digests SET batch_date = '2026-10-09' WHERE id = 'dig1'`)
	f.item(t, "other", "u2", "src9", "light", "")
	f.exec(t, `UPDATE items SET batch_date = '2026-10-09' WHERE id = 'other'`)

	today := f.cardIDs(t, "u1", ListToday)
	for _, id := range []string{"today_light", "today_light_only", "today_deep", "dig1"} {
		if _, ok := today[id]; !ok {
			t.Errorf("today is missing %s: %v", id, today)
		}
	}
	for _, id := range []string{"old_light", "old_pending", "today_waiting", "today_expired", "child", "other"} {
		if _, ok := today[id]; ok {
			t.Errorf("today should not show %s", id)
		}
	}
	if !today["today_light"].CanPromote || today["today_light_only"].CanPromote || !today["today_deep"].CanPromote || today["dig1"].CanPromote {
		t.Errorf("can-promote wrong: %+v", today)
	}
	if today["dig1"].Kind != SubjectDigest || today["today_light"].Kind != SubjectItem {
		t.Errorf("kinds: %+v", today)
	}
	if today["today_light"].Summary != "light" {
		t.Errorf("summary %q", today["today_light"].Summary)
	}

	back := f.cardIDs(t, "u1", ListBacklog)
	if len(back) != 1 || back["old_light"].ID == "" {
		t.Errorf("backlog: %v", back)
	}
	if len(f.cardIDs(t, "u2", ListBacklog)) != 0 {
		t.Error("u2 sees u1's backlog")
	}
}

func TestActionedCardsStayWithAnUndoOnlyWhileHeld(t *testing.T) {
	f := newFixture(t)
	f.item(t, "i1", "u1", "src1", "light", "")
	f.item(t, "i2", "u1", "src1", "light", "")
	f.exec(t, `UPDATE items SET batch_date = '2026-10-08'`) // both in the backlog
	f.exec(t, `UPDATE items SET batch_date = '2026-10-09' WHERE id = 'i2'`)
	f.apply(t, "u1", act("a1", "i1", Archive, f.now), act("a2", "i2", Promote, f.now))

	back := f.cardIDs(t, "u1", ListBacklog)
	if c, ok := back["i1"]; !ok || c.State != "archived" || c.UndoID != "a1" {
		t.Fatalf("held archived card should stay in the backlog with its undo: %+v", back)
	}
	today := f.cardIDs(t, "u1", ListToday)
	if c := today["i2"]; c.State != "pending_deep" || c.UndoID != "a2" {
		t.Fatalf("promoted card: %+v", today)
	}

	f.now = f.now.Add(DefaultHold + time.Minute) // the hold ends
	if _, ok := f.cardIDs(t, "u1", ListBacklog)["i1"]; ok {
		t.Fatal("a sent action leaves the backlog")
	}
	if c := f.cardIDs(t, "u1", ListToday)["i2"]; c.State != "pending_deep" || c.UndoID != "" {
		t.Fatalf("promoted card after the hold: %+v", c)
	}
}
