package karakeep

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/dest"
	kk "github.com/Jolls/Siftstr/internal/dest/karakeep"
	"github.com/Jolls/Siftstr/internal/store"
)

var now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// fakeAPI is a Karakeep with a newest-first unarchived list.
type fakeAPI struct {
	marks    []kk.Bookmark // newest first
	pages    int
	listErr  error
	getErr   map[string]error
	gets     []string
	pageSize int
}

func (f *fakeAPI) ListUnarchived(_ context.Context, cursor string, limit int) (kk.Page, error) {
	if f.listErr != nil {
		return kk.Page{}, f.listErr
	}
	f.pages++
	start := 0
	if cursor != "" {
		start, _ = strconv.Atoi(cursor)
	}
	end := min(start+limit, len(f.marks))
	p := kk.Page{Bookmarks: f.marks[start:end]}
	if end < len(f.marks) {
		p.NextCursor = strconv.Itoa(end)
	}
	return p, nil
}

func (f *fakeAPI) Get(_ context.Context, id string) (kk.Bookmark, error) {
	f.gets = append(f.gets, id)
	if err := f.getErr[id]; err != nil {
		return kk.Bookmark{}, err
	}
	for _, b := range f.marks {
		if b.ID == id {
			return b, nil
		}
	}
	return kk.Bookmark{}, kk.ErrNotFound
}

type fixture struct {
	t   *testing.T
	s   *Syncer
	st  *store.Store
	api *fakeAPI
	now time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cs, err := connections.New(st.DB(), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, st: st, api: &fakeAPI{}, now: now}
	f.s = &Syncer{DB: st.DB(), Conns: cs, New: func(string, string) API { return f.api }, Now: func() time.Time { return f.now }}
	f.exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n'), ('u2','u2','x','n')`)
	f.exec(`INSERT INTO sources (id, user_id, kind, external_id, name, summary_source) VALUES
		('src1','u1','miniflux_feed','1','Feed','karakeep'), ('src2','u1','miniflux_feed','2','Plain','agent')`)
	return f
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.st.DB().Exec(q, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) connect(user string) {
	f.t.Helper()
	tok := "tok"
	if _, err := f.s.Conns.Create(context.Background(), user, connections.Input{Kind: "karakeep", BaseURL: "https://kk.example.com", Secret: &tok}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(q string, args ...any) (n int) {
	f.t.Helper()
	if err := f.st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *fixture) run(user string) Result {
	f.t.Helper()
	res, err := f.s.Run(context.Background(), user)
	if err != nil {
		f.t.Fatalf("run: %v", err)
	}
	return res
}

// mark builds a link bookmark created hoursAgo hours before now.
func mark(id, url string, hoursAgo int, tags ...string) kk.Bookmark {
	return kk.Bookmark{ID: id, Type: "link", URL: url, PageTitle: "Title " + id, Description: "About " + id,
		CreatedAt: now.Add(-time.Duration(hoursAgo) * time.Hour), Tags: tags}
}

// --- ingest ---

func TestIngestsUnarchivedBookmarksAsLightItems(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.marks = []kk.Bookmark{mark("b2", "https://example.com/two", 1), mark("b1", "https://www.youtube.com/watch?v=x", 2)}
	res := f.run("u1")
	if res.Items != 2 {
		t.Fatalf("result = %+v", res)
	}
	var state, day, kid, ext, title, excerpt, media, url string
	if err := f.st.DB().QueryRow(`SELECT state, batch_date, karakeep_id, external_id, title, excerpt, media_type, url FROM items WHERE external_id = 'b2'`).
		Scan(&state, &day, &kid, &ext, &title, &excerpt, &media, &url); err != nil {
		t.Fatal(err)
	}
	if state != "light" || day != "2026-10-11" || kid != "b2" || title != "Title b2" || excerpt != "About b2" || media != "article" || url != "https://example.com/two" {
		t.Fatalf("item = %s %s %s %q %q %s %s", state, day, kid, title, excerpt, media, url)
	}
	if f.count(`SELECT COUNT(*) FROM items WHERE external_id = 'b1' AND media_type = 'video'`) != 1 {
		t.Fatal("a video link should be a video")
	}
	var kind, summary string
	var enabled int
	if err := f.st.DB().QueryRow(`SELECT kind, summary_source, enabled FROM sources WHERE external_id = 'karakeep'`).Scan(&kind, &summary, &enabled); err != nil ||
		kind != "karakeep" || summary != "karakeep" || enabled != 1 {
		t.Fatalf("source = %s %s %d, %v", kind, summary, enabled, err)
	}
	if f.count(`SELECT COUNT(*) FROM outbox`) != 0 {
		t.Fatal("a bookmark Karakeep collected needs no bookmark made for it")
	}
	if res := f.run("u1"); res.Items != 0 || f.count(`SELECT COUNT(*) FROM items`) != 2 {
		t.Fatalf("second run = %+v", res)
	}
}

func TestSkipsWhatItShouldNotOffer(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, karakeep_id, created_at, updated_at)
		VALUES ('itm_a', 'u1', 'src1', '99', 'article', 'https://example.com/known', 't', 'light', 'b_made', 'c', 'c')`)
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		VALUES ('itm_b', 'u1', 'src2', '98', 'article', 'https://example.com/fromrss', 't', 'kept', 'c', 'c')`)
	f.api.marks = []kk.Bookmark{
		mark("b_fresh", "https://example.com/fresh", 1),
		mark("b_sifted", "https://example.com/sifted", 1, "sifted", "ai-tag"),
		mark("b_made", "https://example.com/known", 1),                      // Siftstr made it: known by its bookmark id
		mark("b_dupe", "https://EXAMPLE.com/fromrss/?utm_source=x#frag", 1), // the same page, spelt differently
		{ID: "b_note", Type: "text", CreatedAt: now.Add(-time.Hour)},        // no URL to triage
		{ID: "b_asset", Type: "asset", CreatedAt: now.Add(-time.Hour)},
	}
	res := f.run("u1")
	if res.Items != 1 || res.Skipped != 5 {
		t.Fatalf("result = %+v", res)
	}
	if f.count(`SELECT COUNT(*) FROM items WHERE external_id = 'b_fresh'`) != 1 {
		t.Fatal("the fresh bookmark was not ingested")
	}
}

// A kept item is unarchived and tagged sifted, so the next listing must not
// offer it again.
func TestTheSiftedTagBreaksTheLoop(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.marks = []kk.Bookmark{mark("b1", "https://example.com/a", 1)}
	f.run("u1")
	f.exec(`UPDATE items SET state = 'kept'`)
	f.exec(`DELETE FROM items`) // even if the item were gone (janitor), the tag still stops it
	f.api.marks[0].Tags = []string{"sifted", "kept"}
	if res := f.run("u1"); res.Items != 0 {
		t.Fatalf("a sifted bookmark came back for triage: %+v", res)
	}
}

func TestStopsAtTheFirstBookmarkOlderThanMaxAge(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	for i := 0; i < 60; i++ { // newest first; 14 days is 336 hours, and a bookmark exactly that old still counts
		f.api.marks = append(f.api.marks, mark(fmt.Sprintf("b%02d", i), fmt.Sprintf("https://example.com/%d", i), 300+i))
	}
	res := f.run("u1")
	if res.Items != 37 {
		t.Fatalf("items = %d, want the 37 inside 14 days (%+v)", res.Items, res)
	}
	if f.api.pages > 3 {
		t.Fatalf("walked %d pages: it must stop at the first old bookmark, not read the whole library", f.api.pages)
	}
	f.exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'max_age_days', '0')`)
	if res := f.run("u1"); res.Items != 23 { // no limit now: the rest come in
		t.Fatalf("with no age limit: %+v", res)
	}
}

func TestDisabledKarakeepSourceIsNotIngested(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.marks = []kk.Bookmark{mark("b1", "https://example.com/a", 1)}
	f.run("u1")
	f.exec(`UPDATE sources SET enabled = 0 WHERE kind = 'karakeep'`)
	f.api.marks = append([]kk.Bookmark{mark("b2", "https://example.com/b", 1)}, f.api.marks...)
	if res := f.run("u1"); res.Items != 0 {
		t.Fatalf("a disabled source ingested: %+v", res)
	}
}

func TestNoConnectionAndUserIsolation(t *testing.T) {
	f := newFixture(t)
	if _, err := f.s.Run(context.Background(), "u1"); !errors.Is(err, ErrNoConnection) {
		t.Fatalf("err = %v", err)
	}
	f.connect("u2")
	f.api.marks = []kk.Bookmark{mark("b1", "https://example.com/a", 1)}
	f.run("u2")
	if f.count(`SELECT COUNT(*) FROM items WHERE user_id = 'u1'`) != 0 || f.count(`SELECT COUNT(*) FROM items WHERE user_id = 'u2'`) != 1 {
		t.Fatal("items landed on the wrong user")
	}
	// u1 already has the same URL: u2's known URLs must not hide it from u1.
	f.connect("u1")
	f.run("u1")
	if f.count(`SELECT COUNT(*) FROM items WHERE user_id = 'u1'`) != 1 {
		t.Fatal("another user's items hid this user's bookmark")
	}
}

func TestListFailureChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.listErr = errors.New("down")
	if _, err := f.s.Run(context.Background(), "u1"); err == nil {
		t.Fatal("want an error")
	}
	if f.count(`SELECT COUNT(*) FROM items`) != 0 {
		t.Fatal("items were written")
	}
}

// --- resolving a promoted item's summary ---

// promoted adds a promoted item on the Karakeep path, waiting for Karakeep.
func (f *fixture) promoted(id, src, bookmark string, promotedAgo time.Duration) {
	f.t.Helper()
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, excerpt, state, karakeep_id, promoted_at, batch_date, created_at, updated_at)
		VALUES (?, 'u1', ?, ?, 'article', 'https://example.com/'||?, 't', 'the excerpt', 'pending_deep', NULLIF(?, ''), ?, '2026-10-10', 'c', 'c')`,
		id, src, id, id, bookmark, f.now.Add(-promotedAgo).Format(time.RFC3339))
}

func (f *fixture) itemState(id string) (state, summary, day string) {
	f.t.Helper()
	var s *string
	if err := f.st.DB().QueryRow(`SELECT state, deep_summary, COALESCE(batch_date, '') FROM items WHERE id = ?`, id).Scan(&state, &s, &day); err != nil {
		f.t.Fatal(err)
	}
	if s != nil {
		summary = *s
	}
	return
}

func withSummary(id, text, status string) kk.Bookmark {
	b := mark(id, "https://example.com/"+id, 1)
	b.Summary, b.SummaryStatus = text, status
	return b
}

func TestPromotedItemGetsKarakeepsSummary(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.promoted("itm_a", "src1", "b_a", 20*time.Minute)
	f.api.marks = []kk.Bookmark{withSummary("b_a", "Karakeep wrote this.", kk.SummaryDone)}
	res := f.run("u1")
	if res.Resolved != 1 {
		t.Fatalf("result = %+v", res)
	}
	state, sum, day := f.itemState("itm_a")
	if state != "deep" || sum != "Karakeep wrote this." || day != "2026-10-11" {
		t.Fatalf("item = %s %q on %s: it should come back today with the summary", state, sum, day)
	}
	if f.count(`SELECT COUNT(*) FROM outbox`) != 0 {
		t.Fatal("nothing is written to Karakeep for this")
	}
}

func TestPromotedItemWaitsWhileKarakeepIsStillWorking(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.promoted("itm_a", "src1", "b_a", 20*time.Minute)
	f.api.marks = []kk.Bookmark{withSummary("b_a", "", kk.SummaryPending)}
	if res := f.run("u1"); res.Resolved != 0 {
		t.Fatalf("result = %+v", res)
	}
	if state, _, _ := f.itemState("itm_a"); state != "pending_deep" {
		t.Fatalf("state = %s", state)
	}
}

func TestPromotedItemFallsBackToTheExcerptWhenKarakeepGivesUp(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.promoted("itm_failed", "src1", "b_failed", 5*time.Minute)
	f.promoted("itm_gone", "src1", "b_gone", 5*time.Minute)
	f.promoted("itm_nobookmark", "src1", "", 5*time.Minute)
	f.promoted("itm_slow", "src1", "b_slow", 3*time.Hour) // past the 2 hour default
	f.api.marks = []kk.Bookmark{withSummary("b_failed", "", kk.SummaryFailed), withSummary("b_slow", "", kk.SummaryPending)}
	res := f.run("u1")
	if res.Resolved != 0 || res.GaveUp != 3 {
		t.Fatalf("result = %+v", res)
	}
	for id, want := range map[string]string{"itm_failed": "deep", "itm_gone": "deep", "itm_slow": "deep", "itm_nobookmark": "pending_deep"} {
		if state, sum, _ := f.itemState(id); state != want || sum != "" {
			t.Errorf("%s = %s %q, want %s with no summary", id, state, sum, want)
		}
	}
	// With no bookmark at all it waits out the timeout like the rest.
	f.now = f.now.Add(2 * time.Hour)
	f.run("u1")
	if state, _, _ := f.itemState("itm_nobookmark"); state != "deep" {
		t.Fatalf("itm_nobookmark = %s after the timeout", state)
	}
}

func TestTimeoutIsAUserSetting(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'karakeep_summary_timeout_hours', '6')`)
	f.promoted("itm_a", "src1", "b_a", 3*time.Hour)
	f.api.marks = []kk.Bookmark{withSummary("b_a", "", kk.SummaryPending)}
	f.run("u1")
	if state, _, _ := f.itemState("itm_a"); state != "pending_deep" {
		t.Fatalf("state = %s: the user allowed 6 hours", state)
	}
}

// While the promote can still be undone the item is left alone, so an undo is
// never refused because the summary happened to arrive.
func TestHeldPromoteIsNotResolved(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.promoted("itm_a", "src1", "b_a", time.Minute)
	f.exec(`INSERT INTO actions (action_id, user_id, subject_type, subject_id, kind, client_at, received_at, release_at, status)
		VALUES ('act1', 'u1', 'item', 'itm_a', 'promote', 'n', 'n', 'n', 'held')`)
	f.api.marks = []kk.Bookmark{withSummary("b_a", "ready", kk.SummaryDone)}
	if res := f.run("u1"); res.Resolved != 0 || len(f.api.gets) != 0 {
		t.Fatalf("result = %+v, gets = %v", res, f.api.gets)
	}
	f.exec(`UPDATE actions SET status = 'released'`)
	if res := f.run("u1"); res.Resolved != 1 {
		t.Fatalf("result = %+v", res)
	}
}

func TestOnlyKarakeepPathItemsAreResolvedAndOnlyForTheirUser(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('src9','u2','miniflux_feed','1','Other')`)
	f.promoted("itm_agent", "src2", "b_x", 5*time.Minute) // a normal feed: Claude summarizes it
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, karakeep_id, promoted_at, created_at, updated_at)
		VALUES ('itm_u2', 'u2', 'src9', '1', 'article', 'https://example.com/u2', 't', 'pending_deep', 'b_u2', ?, 'c', 'c')`, now.Format(time.RFC3339))
	f.api.marks = []kk.Bookmark{withSummary("b_x", "not for Claude's items", kk.SummaryDone), withSummary("b_u2", "not for u2", kk.SummaryDone)}
	if res := f.run("u1"); res.Resolved != 0 || len(f.api.gets) != 0 {
		t.Fatalf("result = %+v, gets = %v", res, f.api.gets)
	}
	for _, id := range []string{"itm_agent", "itm_u2"} {
		if state, _, _ := f.itemState(id); state != "pending_deep" {
			t.Errorf("%s = %s", id, state)
		}
	}
}

func TestResolveErrorLeavesTheItemWaitingAndOthersGo(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.promoted("itm_a", "src1", "b_a", 20*time.Minute)
	f.promoted("itm_b", "src1", "b_b", 20*time.Minute)
	f.api.getErr = map[string]error{"b_a": errors.New("boom")}
	f.api.marks = []kk.Bookmark{withSummary("b_b", "fine", kk.SummaryDone)}
	res, err := f.s.Run(context.Background(), "u1")
	if err == nil {
		t.Fatal("the error should be reported")
	}
	if res.Resolved != 1 {
		t.Fatalf("result = %+v", res)
	}
	if state, _, _ := f.itemState("itm_a"); state != "pending_deep" {
		t.Fatalf("itm_a = %s", state)
	}
}

// A card names the site the bookmark came from, not "Karakeep": the crawled
// publisher when there is one, else the host of the link.
func TestBylineIsThePublisherOrTheSite(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	withPub := mark("b1", "https://www.armstrongeconomics.com/a", 1)
	withPub.Publisher = "  Armstrong Economics "
	f.api.marks = []kk.Bookmark{withPub, mark("b2", "https://blog.example.org/post", 2), mark("b3", "https://WWW.Example.com/x", 3)}
	f.run("u1")
	for ext, want := range map[string]string{"b1": "Armstrong Economics", "b2": "blog.example.org", "b3": "example.com"} {
		var got string
		if err := f.st.DB().QueryRow(`SELECT COALESCE(byline, '') FROM items WHERE external_id = ?`, ext).Scan(&got); err != nil || got != want {
			t.Errorf("%s byline = %q (%v), want %q", ext, got, err, want)
		}
	}
}

// Items ingested before bylines existed get one the next time their bookmark
// is listed, even if they have been triaged since.
func TestBylineIsFilledInForItemsThatAlreadyExist(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.marks = []kk.Bookmark{mark("b1", "https://example.com/a", 1)}
	f.run("u1")
	f.exec(`UPDATE items SET byline = NULL, state = 'kept'`)
	f.api.marks[0].Publisher = "Example Weekly"
	f.run("u1")
	var got string
	_ = f.st.DB().QueryRow(`SELECT COALESCE(byline, '') FROM items`).Scan(&got)
	if got != "Example Weekly" {
		t.Fatalf("byline = %q", got)
	}
	// What the user or an earlier run already set is left alone.
	f.api.marks[0].Publisher = "Renamed"
	f.run("u1")
	_ = f.st.DB().QueryRow(`SELECT COALESCE(byline, '') FROM items`).Scan(&got)
	if got != "Example Weekly" {
		t.Fatalf("byline changed to %q", got)
	}
	f.exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('src_u2', 'u2', 'karakeep', 'karakeep', 'Karakeep')`)
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, karakeep_id, created_at, updated_at)
		VALUES ('itm_u2', 'u2', 'src_u2', 'b1', 'article', 'https://example.com/a', 't', 'light', 'b1', 'c', 'c')`)
	f.api.marks[0].Publisher = "Another"
	f.run("u1")
	if f.count(`SELECT COUNT(*) FROM items WHERE user_id = 'u2' AND byline IS NOT NULL`) != 0 {
		t.Fatal("another user's items were touched")
	}
}

// A kept Miniflux item has its bookmark listed too (it is unarchived), but
// its card keeps naming its feed.
func TestBylineBackfillLeavesOtherSourcesItemsAlone(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, karakeep_id, created_at, updated_at)
		VALUES ('itm_feed', 'u1', 'src1', '77', 'article', 'https://example.com/feed', 't', 'kept', 'b1', 'c', 'c')`)
	f.api.marks = []kk.Bookmark{mark("b1", "https://example.com/feed", 1, "sifted")}
	f.run("u1")
	if f.count(`SELECT COUNT(*) FROM items WHERE id = 'itm_feed' AND byline IS NOT NULL`) != 0 {
		t.Fatal("a Miniflux item was given a site name in place of its feed name")
	}
}

// Once the newest bookmarks are all known, the walk stops: it must not read
// the whole library every tick.
func TestStopsAtThePageWhereNothingIsNew(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	for i := 0; i < 100; i++ {
		f.api.marks = append(f.api.marks, mark(fmt.Sprintf("b%03d", i), fmt.Sprintf("https://example.com/%d", i), 1+i/10))
	}
	f.exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'max_age_days', '0')`)
	res := f.run("u1")
	if res.Items != 100 || f.api.pages != 5 {
		t.Fatalf("first run: %+v after %d pages", res, f.api.pages)
	}
	f.api.pages = 0
	res = f.run("u1")
	if res.Items != 0 || f.api.pages != 1 {
		t.Fatalf("second run: %+v after %d pages: nothing is new, so one page is enough", res, f.api.pages)
	}
	f.api.marks = append([]kk.Bookmark{mark("fresh", "https://example.com/fresh", 0)}, f.api.marks...)
	f.api.pages = 0
	if res = f.run("u1"); res.Items != 1 || f.api.pages != 2 {
		t.Fatalf("one new bookmark: %+v after %d pages", res, f.api.pages)
	}
}

func (f *fixture) connStatus() (state, detail, via string) {
	f.t.Helper()
	var st sql.NullString
	if err := f.st.DB().QueryRow(`SELECT check_state, check_detail, check_via FROM connections WHERE kind = 'karakeep'`).Scan(&st, &detail, &via); err != nil {
		f.t.Fatal(err)
	}
	return st.String, detail, via
}

func TestListFailureRecordsConnectionStatus(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.listErr = fmt.Errorf("GET x: %w", &dest.StatusError{Code: 503})
	if _, err := f.s.Run(context.Background(), "u1"); err == nil {
		t.Fatal("want an error")
	}
	if state, detail, via := f.connStatus(); state != "unexpected" || detail != "HTTP 503" || via != "ingest" {
		t.Fatalf("%q %q %q", state, detail, via)
	}
}

func TestDisabledSourceRecordsNothing(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.exec(`INSERT INTO sources (id, user_id, kind, external_id, name, enabled) VALUES ('srckk','u1','karakeep','karakeep','Karakeep',0)`)
	f.run("u1")
	if state, _, _ := f.connStatus(); state != "" {
		t.Fatalf("state = %q", state)
	}
}
