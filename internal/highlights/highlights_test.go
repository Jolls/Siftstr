package highlights

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/connections"
	kk "github.com/Jolls/Siftstr/internal/dest/karakeep"
	"github.com/Jolls/Siftstr/internal/sources"
	"github.com/Jolls/Siftstr/internal/store"
)

var now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// fakeAPI is a Karakeep with a newest-first highlight list and its bookmarks.
type fakeAPI struct {
	hs        []kk.Highlight
	marks     map[string]kk.Bookmark
	listErr   error
	getHLErr  map[string]error
	getBMErr  map[string]error
	listCalls int
	getCalls  []string
	pageLimit []int
}

func (f *fakeAPI) ListHighlights(_ context.Context, cursor string, limit int) (kk.HighlightPage, error) {
	f.listCalls++
	f.pageLimit = append(f.pageLimit, limit)
	if f.listErr != nil {
		return kk.HighlightPage{}, f.listErr
	}
	start := 0
	if cursor != "" {
		start, _ = strconv.Atoi(cursor)
	}
	end := min(start+limit, len(f.hs))
	p := kk.HighlightPage{Highlights: f.hs[start:end]}
	if end < len(f.hs) {
		p.NextCursor = strconv.Itoa(end)
	}
	return p, nil
}

func (f *fakeAPI) GetHighlight(_ context.Context, id string) (kk.Highlight, error) {
	f.getCalls = append(f.getCalls, id)
	if err := f.getHLErr[id]; err != nil {
		return kk.Highlight{}, err
	}
	for _, h := range f.hs {
		if h.ID == id {
			return h, nil
		}
	}
	return kk.Highlight{}, kk.ErrHighlightNotFound
}

func (f *fakeAPI) Get(_ context.Context, id string) (kk.Bookmark, error) {
	if err := f.getBMErr[id]; err != nil {
		return kk.Bookmark{}, err
	}
	if b, ok := f.marks[id]; ok {
		return b, nil
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
	f := &fixture{t: t, st: st, api: &fakeAPI{marks: map[string]kk.Bookmark{}}, now: now}
	f.s = &Syncer{DB: st.DB(), Conns: cs, New: func(string, string) API { return f.api }, Now: func() time.Time { return f.now },
		Rand: func(int) int { return 0 }}
	f.exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n'), ('u2','u2','x','n')`)
	return f
}

func (f *fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.st.DB().Exec(q, args...); err != nil {
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

func (f *fixture) connect(user string) {
	f.t.Helper()
	tok := "tok"
	if _, err := f.s.Conns.Create(context.Background(), user, connections.Input{Kind: "karakeep", BaseURL: "https://kk.example.com", Secret: &tok}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) enable(user string) {
	f.t.Helper()
	in, err := sources.New(f.st.DB()).Ingest(context.Background(), user)
	if err != nil {
		f.t.Fatal(err)
	}
	in.KarakeepHighlights = true
	if err := sources.New(f.st.DB()).SetIngest(context.Background(), user, in); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) run(user string) Result {
	f.t.Helper()
	res, err := f.s.Run(context.Background(), user)
	if err != nil {
		f.t.Fatalf("run: %v", err)
	}
	return res
}

// add puts a highlight with text into the fake, with its bookmark.
func (f *fixture) add(id, text string) {
	f.api.hs = append(f.api.hs, kk.Highlight{ID: id, BookmarkID: "b-" + id, Text: text, Color: "yellow", Note: "note " + id})
	f.api.marks["b-"+id] = kk.Bookmark{ID: "b-" + id, Type: "link", URL: "https://www.example.com/" + id, PageTitle: "Page " + id}
}

func (f *fixture) pick(user, day string) (id, text string, ok bool) {
	f.t.Helper()
	err := f.st.DB().QueryRow(`SELECT highlight_id, text FROM highlight_picks WHERE user_id = ? AND day = ?`, user, day).Scan(&id, &text)
	return id, text, err == nil
}

func TestOffByDefault(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.add("h1", "one")
	if res := f.run("u1"); res != (Result{}) {
		t.Errorf("result = %+v, want nothing done", res)
	}
	if f.api.listCalls != 0 || f.count(`SELECT COUNT(*) FROM highlight_cache`) != 0 {
		t.Error("a user who has not opted in must not be read from Karakeep")
	}
}

func TestNoConnection(t *testing.T) {
	f := newFixture(t)
	f.enable("u1")
	if _, err := f.s.Run(context.Background(), "u1"); !errors.Is(err, ErrNoConnection) {
		t.Errorf("error = %v, want ErrNoConnection", err)
	}
}

func TestRefreshCachesOnlyHighlightsWithText(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	f.add("h1", "one")
	f.add("h2", "   ")
	f.add("h3", "three")
	f.api.hs = append(f.api.hs, kk.Highlight{ID: "h4", BookmarkID: "b4"}) // null text
	res := f.run("u1")
	if !res.Refreshed || res.Added != 2 {
		t.Fatalf("result = %+v, want 2 added", res)
	}
	if n := f.count(`SELECT COUNT(*) FROM highlight_cache WHERE user_id = 'u1'`); n != 2 {
		t.Errorf("cached = %d, want 2", n)
	}
	if f.api.pageLimit[0] != kk.MaxHighlightsPerPage {
		t.Errorf("page size = %d, want %d", f.api.pageLimit[0], kk.MaxHighlightsPerPage)
	}
}

func TestPicksOneAndStoresItWithTheDay(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	f.add("h1", "one")
	res := f.run("u1")
	if !res.Picked {
		t.Fatalf("result = %+v", res)
	}
	var note, color, title, link, byline string
	if err := f.st.DB().QueryRow(`SELECT note, color, bookmark_title, bookmark_url, byline FROM highlight_picks WHERE user_id = 'u1' AND day = '2026-10-11'`).
		Scan(&note, &color, &title, &link, &byline); err != nil {
		t.Fatal(err)
	}
	if note != "note h1" || color != "yellow" || title != "Page h1" || link != "https://www.example.com/h1" || byline != "example.com" {
		t.Errorf("stored pick = %q %q %q %q %q", note, color, title, link, byline)
	}
	if f.count(`SELECT COUNT(*) FROM highlight_cache WHERE shown_on = '2026-10-11'`) != 1 {
		t.Error("the picked highlight is not marked as shown")
	}
}

func TestOnePickPerDayAndNoNetworkOnceItExists(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	f.add("h1", "one")
	f.add("h2", "two")
	f.run("u1")
	first, _, _ := f.pick("u1", "2026-10-11")
	calls := len(f.api.getCalls)
	f.run("u1")
	if again, _, _ := f.pick("u1", "2026-10-11"); again != first || len(f.api.getCalls) != calls {
		t.Error("a second run the same day must keep the pick and read nothing")
	}
	if n := f.count(`SELECT COUNT(*) FROM highlight_picks`); n != 1 {
		t.Errorf("picks = %d, want 1", n)
	}
}

func TestEachDayShowsADifferentHighlightUntilAllAreShown(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	for _, id := range []string{"h1", "h2", "h3"} {
		f.add(id, "text "+id)
	}
	seen := map[string]bool{}
	for d := 0; d < 3; d++ {
		f.now = now.AddDate(0, 0, d)
		f.run("u1")
		id, _, ok := f.pick("u1", f.now.Format("2006-01-02"))
		if !ok || seen[id] {
			t.Fatalf("day %d picked %q (repeat or none)", d, id)
		}
		seen[id] = true
	}
	// Everything has been shown: the record clears and picking continues.
	f.now = now.AddDate(0, 0, 3)
	f.run("u1")
	if _, _, ok := f.pick("u1", f.now.Format("2006-01-02")); !ok {
		t.Error("no pick after every highlight had been shown")
	}
}

func TestDeletedHighlightIsDroppedAndAnotherIsPicked(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	f.add("h1", "one")
	f.add("h2", "two")
	f.run("u1") // caches both; picks h1 (index 0) for today
	f.exec(`DELETE FROM highlight_picks`)
	f.exec(`UPDATE highlight_cache SET shown_on = NULL`)
	f.api.hs = f.api.hs[1:] // h1 is deleted in Karakeep after the cache was built
	f.run("u1")
	if id, _, ok := f.pick("u1", "2026-10-11"); !ok || id != "h2" {
		t.Errorf("pick = %q, %v; want h2", id, ok)
	}
	if f.count(`SELECT COUNT(*) FROM highlight_cache WHERE highlight_id = 'h1'`) != 0 {
		t.Error("the deleted highlight is still cached")
	}
}

func TestWeeklyWalkRemovesDeletedAndAddsNew(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	f.add("h1", "one")
	f.add("h2", "two")
	f.run("u1")
	calls := f.api.listCalls
	f.run("u1") // same hour: not due
	if f.api.listCalls != calls {
		t.Error("walked again before a week had passed")
	}
	f.api.hs = f.api.hs[1:]
	f.add("h3", "three")
	f.now = f.now.Add(RefreshEvery)
	res := f.run("u1")
	if !res.Refreshed || res.Added != 1 || res.Removed != 1 {
		t.Errorf("result = %+v, want 1 added and 1 removed", res)
	}
}

func TestFailedWalkKeepsTheCacheAndStillPicksFromIt(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	f.add("h1", "one")
	f.run("u1")
	f.exec(`DELETE FROM highlight_picks`)
	f.s.refreshed = nil // as after a restart
	f.api.listErr = errors.New("karakeep is down")
	res, err := f.s.Run(context.Background(), "u1")
	if err == nil {
		t.Error("the failed walk should be reported")
	}
	if !res.Picked {
		t.Errorf("result = %+v, want a pick from the stored cache", res)
	}
	if f.count(`SELECT COUNT(*) FROM highlight_cache`) != 1 {
		t.Error("a failed walk changed the cache")
	}
}

func TestIncompleteCacheFailsTheDayQuietly(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	f.api.listErr = errors.New("karakeep is down")
	if _, err := f.s.Run(context.Background(), "u1"); err == nil {
		t.Error("expected the walk error")
	}
	if _, _, ok := f.pick("u1", "2026-10-11"); ok {
		t.Error("there is nothing cached, so there must be no pick")
	}
}

func TestPickKeptWithoutTitleWhenBookmarkCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	f.add("h1", "one")
	f.api.getBMErr = map[string]error{"b-h1": errors.New("timeout")}
	f.run("u1")
	var title, link string
	if err := f.st.DB().QueryRow(`SELECT bookmark_title, bookmark_url FROM highlight_picks WHERE user_id = 'u1'`).Scan(&title, &link); err != nil {
		t.Fatalf("no pick stored: %v", err)
	}
	if title != "" || link != "" {
		t.Errorf("title %q and link %q should be empty", title, link)
	}
}

func TestUsersAreIsolated(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.enable("u1")
	f.add("h1", "one")
	f.run("u1")
	f.connect("u2") // connected, but has not opted in
	f.run("u2")
	if f.count(`SELECT COUNT(*) FROM highlight_cache WHERE user_id = 'u2'`) != 0 || f.count(`SELECT COUNT(*) FROM highlight_picks WHERE user_id = 'u2'`) != 0 {
		t.Error("another user's highlights leaked to a user who did not opt in")
	}
	f.enable("u2")
	f.run("u2")
	if _, _, ok := f.pick("u2", "2026-10-11"); !ok {
		t.Error("u2 should have its own pick once opted in")
	}
	if f.count(`SELECT COUNT(*) FROM highlight_picks`) != 2 {
		t.Error("each user has exactly one pick")
	}
}
