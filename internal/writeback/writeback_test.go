package writeback

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/dest"
	"github.com/Jolls/Siftstr/internal/outbox"
	"github.com/Jolls/Siftstr/internal/runs"
	"github.com/Jolls/Siftstr/internal/store"
	"github.com/Jolls/Siftstr/internal/triage"
)

// sent is one call a fake destination received.
type sent struct {
	kind, user string
	item       dest.Item
}

type fixture struct {
	t     *testing.T
	st    *store.Store
	tr    *triage.Service
	runs  *runs.Service
	conns *connections.Service
	obx   *outbox.Runner
	now   time.Time
	calls []sent
	fail  map[string]error // kind -> error to return
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, st: st, now: time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC), fail: map[string]error{}}
	clock := func() time.Time { return f.now }
	conns, err := connections.New(st.DB(), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.conns = conns
	pl := &Planner{Now: clock}
	f.tr = &triage.Service{DB: st.DB(), Now: clock, OnRelease: pl.OnRelease}
	f.runs = &runs.Service{DB: st.DB(), Now: clock, OnDeep: pl.OnDeep}

	snd := &Sender{Conns: conns, Factories: map[string]dest.Factory{}}
	for _, k := range []string{Miniflux, Karakeep, MeTube} {
		k := k
		snd.Factories[k] = func(baseURL, secret string, _ json.RawMessage) dest.Destination {
			return &fakeDest{f: f, kind: k, base: baseURL}
		}
	}
	f.obx = &outbox.Runner{DB: st.DB(), Now: clock, Handle: snd.Handle, MaxAttempts: 3}

	f.exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n'), ('u2','u2','x','n')`)
	f.exec(`INSERT INTO sources (id, user_id, kind, external_id, name, granularity, max_depth) VALUES
		('src1','u1','miniflux_feed','1','Blog','individual','deep'),
		('src3','u1','miniflux_feed','3','News','digest','light'),
		('srcn','u1','nostr','n','Nostr','individual','deep'),
		('src9','u2','miniflux_feed','1','Other','individual','deep')`)
	f.exec(`INSERT INTO digests (id, user_id, source_id, batch_date, summary, state) VALUES ('dig1','u1','src3','2026-10-09','digest text','light')`)
	return f
}

type fakeDest struct {
	f    *fixture
	kind string
	base string
}

func (d *fakeDest) Name() string { return d.kind }
func (d *fakeDest) Send(_ context.Context, it dest.Item) error {
	if err := d.f.fail[d.kind]; err != nil {
		return err
	}
	user := "u1"
	if d.base == "https://u2.example.com" {
		user = "u2"
	}
	d.f.calls = append(d.f.calls, sent{d.kind, user, it})
	return nil
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.st.DB().Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) connect(user string, kinds ...string) {
	f.t.Helper()
	base := "https://" + user + ".example.com"
	for _, k := range kinds {
		secret := "tok"
		if _, err := f.conns.Create(context.Background(), user, connections.Input{Kind: k, BaseURL: base, Secret: &secret}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) item(id, src, media, state string, ext string) {
	f.t.Helper()
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, light_summary, created_at, updated_at)
		VALUES (?, 'u1', ?, ?, ?, 'https://example.com/'||?, 'Title '||?, ?, 'light text', 'c', 'c')`, id, src, ext, media, id, id, state)
}

// act applies one action and moves the clock past the hold, then releases.
func (f *fixture) act(kind, subj string) string {
	f.t.Helper()
	id := "a_" + kind + "_" + subj + "_" + f.now.Format("150405")
	st := triage.SubjectItem
	if strings.HasPrefix(subj, "dig") {
		st = triage.SubjectDigest
	}
	res, err := f.tr.Apply(context.Background(), "u1", []triage.Action{{ActionID: id, SubjectType: st, SubjectID: subj, Kind: kind, At: f.now}})
	if err != nil || res[0].Status != triage.Applied {
		f.t.Fatalf("apply %s: %+v %v", kind, res, err)
	}
	return id
}

func (f *fixture) release() {
	f.t.Helper()
	f.now = f.now.Add(triage.DefaultHold + time.Minute)
	if _, err := f.tr.ReleaseDue(context.Background(), "u1"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) drain() {
	f.t.Helper()
	if _, err := f.obx.Run(context.Background(), "u1"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) kinds() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c.kind)
	}
	sort.Strings(out)
	return out
}

func (f *fixture) call(kind string) dest.Item {
	f.t.Helper()
	for _, c := range f.calls {
		if c.kind == kind {
			return c.item
		}
	}
	f.t.Fatalf("no %s call in %v", kind, f.kinds())
	return dest.Item{}
}

func (f *fixture) outboxCount() (n int) {
	_ = f.st.DB().QueryRow(`SELECT COUNT(*) FROM outbox`).Scan(&n)
	return
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestArchiveMarksReadOnly(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep, MeTube)
	f.item("i1", "src1", "article", "light", "41")
	f.act("archive", "i1")
	if f.outboxCount() != 0 {
		t.Fatal("effects were queued before the hold ended")
	}
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"miniflux"})
	if got := f.call("miniflux").EntryIDs; !slices.Equal(got, []int64{41}) {
		t.Fatalf("entry ids %v", got)
	}
}

func TestReplayingTheOutboxIsHarmless(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("keep", "i1")
	f.release()
	f.drain()
	f.drain()
	if _, err := f.tr.ReleaseDue(context.Background(), "u1"); err != nil { // nothing left to release
		t.Fatal(err)
	}
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "miniflux"})
	if f.outboxCount() != 2 {
		t.Fatalf("%d outbox rows", f.outboxCount())
	}
}

func TestKeepArticleGoesToKarakeepUnarchivedWithSummary(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep, MeTube)
	f.item("i1", "src1", "article", "light", "41")
	f.act("keep", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "miniflux"})
	k := f.call("karakeep")
	if k.Archived || k.Summary != "light text" || !slices.Equal(k.Tags, []string{"sifted", "kept"}) || k.URL != "https://example.com/i1" {
		t.Fatalf("%+v", k)
	}
}

func TestKeepVideoFollowsTheMultiSelect(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep, MeTube)
	f.item("i1", "src1", "video", "light", "41")
	f.item("i2", "src1", "video", "light", "42")
	f.act("keep", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "metube", "miniflux"}) // default: every connected one

	f.calls = nil
	f.exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'video_destinations', 'metube')`)
	f.act("keep", "i2")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"metube", "miniflux"})
	if f.call("metube").Tags != nil {
		t.Fatal("tags are for Karakeep only")
	}
}

func TestKeepPodcastDefaultsToKarakeepAndCanAddMore(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep, MeTube)
	f.item("i1", "src1", "podcast", "light", "41")
	f.act("keep", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep"})

	f.calls = nil
	f.exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'podcast_destinations', 'karakeep, metube, bogus')`)
	f.item("i2", "src1", "podcast", "light", "42")
	f.act("keep", "i2")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "metube"})
}

func TestPromoteMarksReadNowAndWaitsForTheDeepSummary(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"miniflux"}) // Q10: read at promote; Karakeep waits

	// The morning run delivers the deep summary.
	f.exec(`INSERT INTO runs (id, user_id, started_at, day) VALUES ('run1','u1','n','2026-10-10')`)
	f.exec(`INSERT INTO run_items (run_id, user_id, subject_id) VALUES ('run1','u1','i1')`)
	res, err := f.runs.Submit(context.Background(), "u1", "run1", []runs.Summary{{ID: "i1", Stage: "deep", MD: "deep text"}})
	if err != nil || res.Accepted != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "miniflux"})
	k := f.call("karakeep")
	if !k.Archived || k.Summary != "deep text" || !slices.Equal(k.Tags, []string{"sifted", "promoted"}) {
		t.Fatalf("%+v", k)
	}

	// Submitting the same batch again, or draining again, sends nothing more.
	_, _ = f.runs.Submit(context.Background(), "u1", "run1", []runs.Summary{{ID: "i1", Stage: "deep", MD: "deep text"}})
	f.drain()
	if len(f.calls) != 2 || f.outboxCount() != 2 {
		t.Fatalf("%d calls, %d rows", len(f.calls), f.outboxCount())
	}
}

func TestDeepSummaryArrivingDuringTheHoldIsSavedAtRelease(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.exec(`INSERT INTO runs (id, user_id, started_at, day) VALUES ('run1','u1','n','2026-10-10')`)
	f.exec(`INSERT INTO run_items (run_id, user_id, subject_id) VALUES ('run1','u1','i1')`)
	if _, err := f.runs.Submit(context.Background(), "u1", "run1", []runs.Summary{{ID: "i1", Stage: "deep", MD: "deep text"}}); err != nil {
		t.Fatal(err)
	}
	if f.outboxCount() != 0 {
		t.Fatal("saved while the promote was still undoable")
	}
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep"})
	if f.call("karakeep").Summary != "deep text" {
		t.Fatalf("%+v", f.call("karakeep"))
	}
}

func TestKeepAfterPromoteUnarchivesInKarakeep(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("promote", "i1")
	f.release()
	f.exec(`INSERT INTO runs (id, user_id, started_at, day) VALUES ('run1','u1','n','2026-10-10')`)
	f.exec(`INSERT INTO run_items (run_id, user_id, subject_id) VALUES ('run1','u1','i1')`)
	_, _ = f.runs.Submit(context.Background(), "u1", "run1", []runs.Summary{{ID: "i1", Stage: "deep", MD: "deep text"}})
	f.drain()
	f.calls = nil
	f.now = f.now.Add(time.Hour)
	f.act("keep", "i1")
	f.release()
	f.drain()
	k := f.call("karakeep")
	if k.Archived || k.Summary != "deep text" {
		t.Fatalf("%+v", k)
	}
}

func TestUndoWithinTheHoldQueuesNothing(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	id := f.act("keep", "i1")
	res, _ := f.tr.Apply(context.Background(), "u1", []triage.Action{{ActionID: "u1", SubjectID: "i1", Kind: "undo", TargetActionID: id, At: f.now}})
	if res[0].Status != triage.Applied {
		t.Fatalf("%+v", res)
	}
	f.release()
	f.drain()
	if f.outboxCount() != 0 || len(f.calls) != 0 {
		t.Fatalf("%d rows, %v", f.outboxCount(), f.kinds())
	}
}

func TestNostrItemsHaveNoMiniflux(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.item("i1", "srcn", "post", "light", "abc")
	f.act("archive", "i1")
	f.release()
	f.drain()
	if len(f.calls) != 0 {
		t.Fatalf("archive of a nostr item sent %v", f.kinds())
	}
	f.item("i2", "srcn", "post", "light", "def")
	f.act("keep", "i2")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep"})
}

func TestDigestArchiveAndKeep(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, digest_id, created_at, updated_at) VALUES
		('c1','u1','src3','11','article','https://example.com/c1','c1','light','dig1','c','c'),
		('c2','u1','src3','12','article','https://example.com/c2','c2','light','dig1','c','c')`)
	f.act("keep", "dig1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "miniflux"})
	if got := f.call("miniflux").EntryIDs; !slices.Equal(got, []int64{11, 12}) {
		t.Fatalf("children %v", got)
	}
	k := f.call("karakeep")
	if !k.Note || k.Summary != "digest text" || k.URL != "" || k.Title != "News" {
		t.Fatalf("%+v", k)
	}
}

func TestNothingConnectedStillReleases(t *testing.T) {
	f := newFixture(t)
	f.item("i1", "src1", "article", "light", "41")
	id := f.act("keep", "i1")
	f.release()
	var status string
	_ = f.st.DB().QueryRow(`SELECT status FROM actions WHERE action_id = ?`, id).Scan(&status)
	if status != "released" || f.outboxCount() != 0 {
		t.Fatalf("%s, %d rows", status, f.outboxCount())
	}
}

func TestFailedPlanKeepsTheActionHeld(t *testing.T) {
	f := newFixture(t)
	f.item("i1", "src1", "article", "light", "41")
	id := f.act("keep", "i1")
	f.tr.OnRelease = func(context.Context, *sql.Tx, string, []triage.Released) error { return errors.New("boom") }
	f.now = f.now.Add(triage.DefaultHold + time.Minute)
	if _, err := f.tr.ReleaseDue(context.Background(), "u1"); err == nil {
		t.Fatal("want error")
	}
	var status string
	_ = f.st.DB().QueryRow(`SELECT status FROM actions WHERE action_id = ?`, id).Scan(&status)
	if status != "held" {
		t.Fatalf("status %s: the action was released without its effects", status)
	}
}

func TestFailingDestinationRetriesWithoutResendingTheOthers(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.fail[Karakeep] = errors.New("down")
	f.act("keep", "i1")
	f.release()
	f.drain()
	eq(t, f.kinds(), []string{"miniflux"})
	delete(f.fail, Karakeep)
	f.now = f.now.Add(outbox.Backoff(1) + time.Second)
	f.drain()
	eq(t, f.kinds(), []string{"karakeep", "miniflux"})
}

func TestRemovedConnectionFailsPermanently(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.item("i1", "src1", "article", "light", "41")
	f.act("keep", "i1")
	f.release()
	f.exec(`DELETE FROM connections WHERE kind = 'karakeep'`)
	f.drain()
	var status, lastErr string
	_ = f.st.DB().QueryRow(`SELECT status, last_error FROM outbox WHERE op = 'karakeep'`).Scan(&status, &lastErr)
	if status != "failed" || lastErr == "" {
		t.Fatalf("%s %q", status, lastErr)
	}
}

func TestUsersNeverReachEachOthersConnections(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Karakeep)
	f.connect("u2", Karakeep, Miniflux)
	f.item("i1", "src1", "article", "light", "41")
	f.act("keep", "i1")
	f.release()
	f.drain()
	for _, c := range f.calls {
		if c.user != "u1" {
			t.Fatalf("u1's item went to %s's connection", c.user)
		}
	}
	eq(t, f.kinds(), []string{"karakeep"}) // u1 has no Miniflux connection
}

func TestConnectionWithoutSecretFailsAtOnce(t *testing.T) {
	f := newFixture(t)
	f.connect("u1", Miniflux, Karakeep)
	f.exec(`UPDATE connections SET secret_enc = NULL WHERE kind = 'karakeep'`)
	f.item("i1", "src1", "article", "light", "41")
	f.act("keep", "i1")
	f.release()
	f.drain()
	var status, lastErr string
	_ = f.st.DB().QueryRow(`SELECT status, last_error FROM outbox WHERE op = 'karakeep'`).Scan(&status, &lastErr)
	if status != "failed" || !strings.Contains(lastErr, "no secret") {
		t.Fatalf("%s %q", status, lastErr)
	}
}
