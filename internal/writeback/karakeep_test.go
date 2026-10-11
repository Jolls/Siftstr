package writeback

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/dest"
	"github.com/Jolls/Siftstr/internal/runs"
	"github.com/Jolls/Siftstr/internal/triage"
)

// deliver stands in for the morning run delivering a deep summary.
func (f *fixture) deliver(item, summary string) {
	f.t.Helper()
	f.exec(`INSERT OR IGNORE INTO runs (id, user_id, started_at, day) VALUES ('run1','u1','n','2026-10-10')`)
	f.exec(`INSERT OR IGNORE INTO run_items (run_id, user_id, subject_id) VALUES ('run1','u1',?)`, item)
	res, err := f.runs.Submit(context.Background(), "u1", "run1", []runs.Summary{{ID: item, Stage: "deep", MD: summary}})
	if err != nil || res.Accepted != 1 {
		f.t.Fatalf("submit: %+v %v", res, err)
	}
}

func (f *fixture) bookmarkID(item string) (id string) {
	f.t.Helper()
	_ = f.st.DB().QueryRow(`SELECT COALESCE(karakeep_id, '') FROM items WHERE id = ?`, item).Scan(&id)
	return
}

func (f *fixture) summaryCalls() (n int) {
	for _, c := range f.calls {
		if c.item.Verb == dest.VerbSummary {
			n++
		}
	}
	return
}

// --- the agent path: Claude summarizes a promoted item ---

func TestPromoteSavesTheBookmarkAtReleaseAndOverwritesTheSummaryAfterTheDelay(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "miniflux"}) // Q10: read at promote, and the bookmark exists from then
	k := f.call("karakeep")
	if k.Verb != dest.VerbSave || !k.Archived || k.Summary != "" || !slices.Equal(k.Tags, []string{"sifted", "promoted"}) {
		t.Fatalf("the bookmark is created archived and tagged, with no summary of ours yet: %+v", k)
	}
	if got := f.bookmarkID("i1"); got != "bm_i1" {
		t.Fatalf("bookmark id recorded as %q", got)
	}

	f.deliver("i1", "deep text") // the morning run
	f.drain()
	if f.summaryCalls() != 0 {
		t.Fatal("Karakeep's own summarizer is still running: ours must wait out the delay")
	}
	f.now = f.now.Add(DefaultSummaryDelay - 2*time.Minute)
	f.drain()
	if f.summaryCalls() != 0 {
		t.Fatal("sent before the delay ended")
	}
	f.now = f.now.Add(3 * time.Minute)
	f.drain()
	if f.summaryCalls() != 1 {
		t.Fatalf("calls: %v", f.kinds())
	}
	var s dest.Item
	for _, c := range f.calls {
		if c.item.Verb == dest.VerbSummary {
			s = c.item
		}
	}
	if s.Summary != "deep text" || s.BookmarkID != "bm_i1" {
		t.Fatalf("%+v", s)
	}

	// A resubmission or another drain changes nothing.
	_, _ = f.runs.Submit(context.Background(), "u1", "run1", []runs.Summary{{ID: "i1", Stage: "deep", MD: "deep text"}})
	f.now = f.now.Add(time.Hour)
	f.drain()
	if f.summaryCalls() != 1 {
		t.Fatalf("%d summary writes", f.summaryCalls())
	}
}

// The delay counts from when the bookmark was created, so a summary that
// arrives long after goes straight out.
func TestSummaryArrivingLateIsNotDelayedAgain(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.release()
	f.drain()
	f.now = f.now.Add(10 * time.Hour)
	f.deliver("i1", "deep text")
	f.drain()
	if f.summaryCalls() != 1 {
		t.Fatalf("calls: %v", f.kinds())
	}
}

func TestSummaryDelayIsAUserSetting(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'karakeep_summary_delay_minutes', '5')`)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.release()
	f.drain()
	f.deliver("i1", "deep text")
	f.now = f.now.Add(6 * time.Minute)
	f.drain()
	if f.summaryCalls() != 1 {
		t.Fatalf("calls: %v", f.kinds())
	}
}

func TestDeepSummaryArrivingDuringTheHoldIsQueuedAtRelease(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.deliver("i1", "deep text")
	if f.outboxCount() != 0 {
		t.Fatal("queued while the promote was still undoable")
	}
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep"}) // the bookmark; the summary is not due yet
	f.now = f.now.Add(DefaultSummaryDelay + time.Minute)
	f.drain()
	if f.summaryCalls() != 1 {
		t.Fatalf("calls: %v", f.kinds())
	}
}

func TestKeepAfterPromoteUnarchivesInKarakeepAndWritesNoSummary(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.release()
	f.deliver("i1", "deep text")
	f.now = f.now.Add(time.Hour)
	f.drain()
	f.calls = nil
	f.act("keep", "i1")
	f.release()
	f.drain()
	k := f.call("karakeep")
	if k.Archived || k.Summary != "" || k.Verb != dest.VerbSave || !slices.Equal(k.Tags, []string{"sifted", "kept"}) {
		t.Fatalf("%+v", k)
	}
}

func TestArchiveAfterPromoteLeavesTheBookmarkAlone(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.release()
	f.deliver("i1", "deep text")
	f.now = f.now.Add(time.Hour)
	f.drain()
	f.calls = nil
	f.act("archive", "i1")
	f.release()
	f.drain()
	for _, c := range f.calls {
		if c.kind == Karakeep {
			t.Fatalf("archiving a promoted item touched Karakeep: %+v", c.item)
		}
	}
}

// --- the Karakeep path: Karakeep summarizes at ingest ---

func (f *fixture) karakeepItem(id, src, state string) {
	f.t.Helper()
	f.item(id, src, "article", state, "41")
	f.exec(`UPDATE items SET karakeep_id = 'bm_' || id WHERE id = ?`, id)
}

func (f *fixture) lastKarakeep() dest.Item {
	f.t.Helper()
	var last dest.Item
	found := false
	for _, c := range f.calls {
		if c.kind == Karakeep {
			last, found = c.item, true
		}
	}
	if !found {
		f.t.Fatalf("no karakeep call in %v", f.kinds())
	}
	return last
}

func TestDeleteRemovesTheKarakeepBookmarkOfAKarakeepSummarySource(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.karakeepItem("i1", "srck", "light")
	f.act("archive", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "miniflux"})
	if k := f.call("karakeep"); k.Verb != dest.VerbDelete || k.BookmarkID != "bm_i1" {
		t.Fatalf("%+v", k)
	}
}

func TestDeleteRemovesBookmarksKarakeepCollectedItself(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.karakeepItem("i1", "srckk", "light")
	f.act("archive", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep"}) // no Miniflux entry for it
	if k := f.call("karakeep"); k.Verb != dest.VerbDelete || k.BookmarkID != "bm_i1" {
		t.Fatalf("%+v", k)
	}
}

func TestArchiveOfAnAgentItemDeletesNothing(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("archive", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"miniflux"})
}

func TestKarakeepSummarySourceWithoutAConnectionBehavesAsAgent(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux)
	f.karakeepItem("i1", "srck", "light")
	f.act("archive", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"miniflux"})
}

func TestPromoteOnTheKarakeepPathTagsAndArchivesWithNoSummaryEntry(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.karakeepItem("i1", "srck", "light")
	f.act("promote", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "miniflux"})
	k := f.call("karakeep")
	if k.Verb != dest.VerbUpdate || !k.Archived || k.Summary != "" || !slices.Equal(k.Tags, []string{"sifted", "promoted"}) || k.BookmarkID != "bm_i1" {
		t.Fatalf("%+v", k)
	}
	f.now = f.now.Add(24 * time.Hour)
	f.drain()
	if f.summaryCalls() != 0 {
		t.Fatal("Siftstr never writes a summary on this path: Karakeep's own stands")
	}
}

func TestArchiveAfterPromoteOnTheKarakeepPathKeepsTheBookmark(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.karakeepItem("i1", "srck", "deep")
	f.exec(`UPDATE items SET promoted_at = '2026-10-09T07:00:00Z', deep_summary = 'karakeep text' WHERE id = 'i1'`)
	f.act("archive", "i1")
	f.release()
	f.drain()
	if len(f.calls) != 0 {
		t.Fatalf("calls: %v", f.kinds())
	}
}

func TestKeepOnTheKarakeepPathUnarchivesAndTags(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.karakeepItem("i1", "srck", "light")
	f.act("keep", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "miniflux"})
	k := f.call("karakeep")
	if k.Verb != dest.VerbUpdate || k.Archived || !slices.Equal(k.Tags, []string{"sifted", "kept"}) || k.BookmarkID != "bm_i1" {
		t.Fatalf("%+v", k)
	}
}

// A kept video whose user does not want Karakeep as a video destination does
// not stay in Karakeep: the bookmark was only there to be summarized, unless
// the user saved it there themselves.
func TestKeepVideoWithoutKarakeepDeletesTheBookmarkSiftstrMade(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep, MeTube)
	f.exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'video_destinations', 'metube')`)
	f.item("i1", "srck", "video", "light", "41")
	f.exec(`UPDATE items SET karakeep_id = 'bm_i1' WHERE id = 'i1'`)
	f.item("i2", "srckk", "video", "light", "42")
	f.exec(`UPDATE items SET karakeep_id = 'bm_i2' WHERE id = 'i2'`)
	f.act("keep", "i1")
	f.act("keep", "i2")
	f.release()
	f.drain()
	byID := map[string]dest.Item{}
	for _, c := range f.calls {
		if c.kind == Karakeep {
			byID[c.item.ID] = c.item
		}
	}
	if byID["i1"].Verb != dest.VerbDelete || byID["i1"].BookmarkID != "bm_i1" {
		t.Errorf("siftstr-made bookmark: %+v", byID["i1"])
	}
	if k := byID["i2"]; k.Verb != dest.VerbUpdate || k.Archived || !slices.Equal(k.Tags, []string{"sifted", "kept"}) {
		t.Errorf("a bookmark the user saved is only tagged and unarchived: %+v", k)
	}
}

func TestSenderResolvesTheBookmarkAtSendTime(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.karakeepItem("i1", "srck", "light")
	f.exec(`UPDATE items SET karakeep_id = NULL WHERE id = 'i1'`) // the ingest entry has not been sent yet
	f.act("promote", "i1")
	f.release()
	f.drain()
	k := f.lastKarakeep()
	if k.Verb != dest.VerbSave || k.URL != "https://example.com/i1" || !k.Archived || !slices.Equal(k.Tags, []string{"sifted", "promoted"}) {
		t.Fatalf("with no bookmark yet an update falls back to a save by URL: %+v", k)
	}
	if got := f.bookmarkID("i1"); got != "bm_i1" {
		t.Fatalf("recorded %q", got)
	}
}

func TestDeleteBeforeTheBookmarkExistsIsANoOp(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.karakeepItem("i1", "srck", "light")
	f.exec(`UPDATE items SET karakeep_id = NULL WHERE id = 'i1'`)
	f.act("archive", "i1")
	f.release()
	f.drain()
	if len(f.calls) != 0 {
		t.Fatalf("calls: %v", f.kinds())
	}
	var status string
	_ = f.st.DB().QueryRow(`SELECT status FROM outbox WHERE op = 'karakeep'`).Scan(&status)
	if status != "done" {
		t.Fatalf("entry is %q: nothing to delete is not a failure", status)
	}
}

// An ingest-time bookmark that is sent after the user has already decided
// must not re-archive what a keep unarchived.
func TestIngestBookmarkIsSkippedOnceTheItemMovedOn(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.item("i1", "srck", "article", "light", "41")
	f.queueIngest("i1")
	f.exec(`UPDATE items SET state = 'kept' WHERE id = 'i1'`)
	f.drain()
	if len(f.calls) != 0 {
		t.Fatalf("the late ingest entry went out: %v", f.kinds())
	}
	f.item("i2", "srck", "article", "light", "42")
	f.queueIngest("i2")
	f.drain()
	if k := f.call("karakeep"); k.ID != "i2" || !k.Archived || k.Verb != dest.VerbCreate {
		t.Fatalf("%+v", k)
	}
	if got := f.bookmarkID("i2"); got != "bm_i2" {
		t.Fatalf("recorded %q", got)
	}
}

func (f *fixture) queueIngest(item string) {
	f.t.Helper()
	f.exec(`INSERT INTO outbox (id, user_id, connection_id, op, payload, status, attempts, created_at, next_attempt_at, dedupe_key)
		SELECT 'obx_' || ?, 'u1', id, 'karakeep',
		  '{"id":"' || ? || '","url":"https://example.com/' || ? || '","archived":true,"verb":"create"}', 'pending', 0, 'n', '', 'karakeep-ingest:' || ?
		FROM connections WHERE user_id = 'u1' AND kind = 'karakeep'`, item, item, item, item)
}

func TestBookmarkIdsAreRecordedPerUser(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		VALUES ('i1', 'u2', 'src9', '1', 'article', 'https://example.com/theirs', 't', 'light', 'c', 'c')`)
	f.queueIngest("i1") // an entry for u1 that names u2's item
	f.drain()
	var theirs string
	_ = f.st.DB().QueryRow(`SELECT COALESCE(karakeep_id, '') FROM items WHERE id = 'i1' AND user_id = 'u2'`).Scan(&theirs)
	if theirs != "" {
		t.Fatal("recorded a bookmark on another user's item")
	}
}

// Deleting a Karakeep item removes a bookmark for good, so it gets the same
// undo window as every other action: nothing is queued until the hold ends,
// and an undo inside it means Karakeep is never touched.
func TestKarakeepDeleteHasTheSameUndoWindow(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.karakeepItem("i1", "srckk", "light")
	f.item("i2", "srckk", "article", "light", "42")
	f.exec(`UPDATE items SET karakeep_id = 'bm_i2' WHERE id = 'i2'`)
	id1 := f.act("archive", "i1")
	f.act("archive", "i2")

	f.now = f.now.Add(triage.DefaultHold - time.Minute) // still inside the window
	if _, err := f.tr.ReleaseDue(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	f.drain()
	if f.outboxCount() != 0 || len(f.calls) != 0 {
		t.Fatalf("effects went out inside the undo window: %d queued, %v sent", f.outboxCount(), f.kinds())
	}
	res, err := f.tr.Apply(context.Background(), "u1", []triage.Action{{ActionID: "undo1", SubjectID: "i1", Kind: triage.Undo, TargetActionID: id1, At: f.now}})
	if err != nil || res[0].Status != triage.Applied {
		t.Fatalf("undo inside the window: %+v %v", res, err)
	}
	f.release()
	f.drain()
	if len(f.calls) != 1 || f.calls[0].item.ID != "i2" || f.calls[0].item.Verb != dest.VerbDelete {
		t.Fatalf("only the item that was not undone should be deleted: %+v", f.calls)
	}
	if state, _ := f.itemStateOf("i1"); state != "light" {
		t.Fatalf("the undone item is %s", state)
	}
}

func (f *fixture) itemStateOf(id string) (state string, ok bool) {
	err := f.st.DB().QueryRow(`SELECT state FROM items WHERE id = ?`, id).Scan(&state)
	return state, err == nil
}

// A summary whose bookmark is not made yet waits, and does not run out of
// retries while the bookmark is slow.
func TestSummaryWaitsForTheBookmarkWithoutFailing(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.release()
	f.fail[Karakeep] = errors.New("down") // the bookmark cannot be made
	f.drain()
	f.deliver("i1", "deep text")
	for i := 0; i < 15; i++ { // the summary comes due, and finds no bookmark
		f.now = f.now.Add(DefaultSummaryDelay)
		f.drain()
	}
	var status string
	_ = f.st.DB().QueryRow(`SELECT status FROM outbox WHERE dedupe_key = 'karakeep-summary:i1'`).Scan(&status)
	if status != "pending" {
		t.Fatalf("the summary entry is %s: waiting for the bookmark must not use up its retries", status)
	}
}

func TestSummaryWithoutAPromoteEntryWaitsTheFullDelay(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.release()
	f.exec(`DELETE FROM outbox WHERE dedupe_key = 'karakeep-promote:i1'`) // as if its time could not be found
	f.exec(`UPDATE items SET karakeep_id = 'bm_i1' WHERE id = 'i1'`)
	f.deliver("i1", "deep text")
	f.drain()
	if f.summaryCalls() != 0 {
		t.Fatal("the summary went out at once: Karakeep's own summarizer could overwrite it")
	}
	f.now = f.now.Add(DefaultSummaryDelay + time.Minute)
	f.drain()
	if f.summaryCalls() != 1 {
		t.Fatalf("calls: %v", f.kinds())
	}
}
