package oldsifting

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/connections"
	kk "github.com/Jolls/Siftstr/internal/dest/karakeep"
	ingestkarakeep "github.com/Jolls/Siftstr/internal/ingest/karakeep"
	"github.com/Jolls/Siftstr/internal/store"
	"github.com/Jolls/Siftstr/internal/triage"
)

var now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

const today = "2026-10-11"

// fakeAPI is a Karakeep with a newest-first unarchived list.
type fakeAPI struct {
	marks     []kk.Bookmark // newest first; archived ones are not listed
	cursorErr map[string]error
	listErr   error
	getErr    map[string]error
	cursors   []string
	gets      []string
}

func (f *fakeAPI) ListUnarchived(_ context.Context, cursor string, limit int) (kk.Page, error) {
	f.cursors = append(f.cursors, cursor)
	if f.listErr != nil {
		return kk.Page{}, f.listErr
	}
	if err := f.cursorErr[cursor]; err != nil {
		return kk.Page{}, err
	}
	var live []kk.Bookmark
	for _, b := range f.marks {
		if !b.Archived {
			live = append(live, b)
		}
	}
	start := 0
	if cursor != "" {
		start, _ = strconv.Atoi(cursor)
	}
	start = min(start, len(live))
	end := min(start+limit, len(live))
	p := kk.Page{Bookmarks: live[start:end]}
	if end < len(live) {
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
	f.s = &Syncer{DB: st.DB(), Conns: cs, New: func(string, string) ingestkarakeep.API { return f.api }, Now: func() time.Time { return f.now }}
	f.exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n'), ('u2','u2','x','n')`)
	f.exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('srcM','u1','miniflux_feed','1','Feed'), ('srcM2','u2','miniflux_feed','1','Feed')`)
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

func (f *fixture) set(user, key, value string) {
	f.t.Helper()
	f.exec(`INSERT INTO user_settings (user_id, key, value) VALUES (?, ?, ?) ON CONFLICT (user_id, key) DO UPDATE SET value = excluded.value`, user, key, value)
}

func (f *fixture) setting(user, key string) string {
	f.t.Helper()
	var v string
	err := f.st.DB().QueryRow(`SELECT value FROM user_settings WHERE user_id = ? AND key = ?`, user, key).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		f.t.Fatal(err)
	}
	return v
}

func (f *fixture) count(q string, args ...any) (n int) {
	f.t.Helper()
	if err := f.st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *fixture) cache(user string) []string {
	f.t.Helper()
	rows, err := f.st.DB().Query(`SELECT bookmark_id FROM old_bookmark_cache WHERE user_id = ? ORDER BY bookmark_id`, user)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

func (f *fixture) items(user string) []string {
	f.t.Helper()
	rows, err := f.st.DB().Query(`SELECT i.external_id FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id
		WHERE i.user_id = ? AND s.kind = 'karakeep' ORDER BY i.external_id`, user)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (f *fixture) run(user string) Result {
	f.t.Helper()
	res, err := f.s.Run(context.Background(), user)
	if err != nil {
		f.t.Fatalf("run: %v", err)
	}
	return res
}

// old builds a link bookmark created daysAgo days before now.
func old(id string, daysAgo int) kk.Bookmark {
	return kk.Bookmark{ID: id, Type: "link", URL: "https://example.com/" + id, PageTitle: "Title " + id, Description: "About " + id,
		Publisher: "Example", CreatedAt: now.Add(-time.Duration(daysAgo) * 24 * time.Hour)}
}

func recent(id string, hoursAgo int) kk.Bookmark {
	b := old(id, 0)
	b.CreatedAt = now.Add(-time.Duration(hoursAgo) * time.Hour)
	return b
}

// oldRange is n old bookmarks, o000 the newest and the last one the oldest.
func oldRange(n int) []kk.Bookmark {
	var out []kk.Bookmark
	for i := 0; i < n; i++ {
		out = append(out, old(fmt.Sprintf("o%03d", i), 20+i))
	}
	return out
}

func (f *fixture) backlogItems(n int) {
	f.t.Helper()
	for i := 0; i < n; i++ {
		f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, batch_date, created_at, updated_at)
			VALUES (?, 'u1', 'srcM', ?, 'article', ?, 't', 'light', '2026-10-01', 'c', 'c')`, fmt.Sprintf("bl%d", i), fmt.Sprintf("bl%d", i), fmt.Sprintf("https://blog.example.com/%d", i))
	}
}

func TestOffWhenDailyIsZero(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.connect("u2")
	f.set("u2", "old_sifting_daily", "0")
	f.api.marks = oldRange(30)
	if res := f.run("u1"); res.Pulled != 5 {
		t.Fatalf("u1 result = %+v", res)
	}
	f.api.cursors, f.api.gets = nil, nil
	if res := f.run("u2"); res != (Result{}) {
		t.Fatalf("u2 result = %+v", res)
	}
	if len(f.api.cursors) != 0 || len(f.api.gets) != 0 || len(f.cache("u2")) != 0 || len(f.items("u2")) != 0 {
		t.Fatalf("u2 touched Karakeep or the database: %v %v", f.api.cursors, f.api.gets)
	}
}

func TestOffWithNoAgeLimitOrDisabledSource(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.marks = oldRange(30)
	f.set("u1", "max_age_days", "0")
	f.run("u1")
	if len(f.api.cursors) != 0 {
		t.Fatalf("no age limit still listed: %v", f.api.cursors)
	}
	f.set("u1", "max_age_days", "14")
	f.exec(`INSERT INTO sources (id, user_id, kind, external_id, name, enabled) VALUES ('kk1','u1','karakeep','karakeep','Karakeep',0)`)
	f.run("u1")
	if len(f.api.cursors) != 0 {
		t.Fatalf("disabled source still listed: %v", f.api.cursors)
	}
	f.exec(`UPDATE sources SET enabled = 1 WHERE id = 'kk1'`)
	if res := f.run("u1"); res.Reads == 0 {
		t.Fatalf("enabled source did nothing: %+v", res)
	}
}

func TestNoKarakeepConnection(t *testing.T) {
	f := newFixture(t)
	if _, err := f.s.Run(context.Background(), "u1"); !errors.Is(err, ErrNoConnection) {
		t.Fatalf("err = %v", err)
	}
	f.set("u1", "old_sifting_daily", "0")
	if _, err := f.s.Run(context.Background(), "u1"); err != nil {
		t.Fatalf("off with no connection: %v", err)
	}
}

func TestCrawlCachesOnlyOldQualifyingBookmarks(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.set("u1", "old_sifting_backlog_cap", "0")
	var marks []kk.Bookmark
	for i := 0; i < 25; i++ {
		marks = append(marks, recent(fmt.Sprintf("r%02d", i), i+1))
	}
	sifted := old("o_sifted", 40)
	sifted.Tags = []string{"sifted"}
	text := old("o_note", 41)
	text.Type = "text"
	marks = append(marks, old("o_plain1", 30), old("o_plain2", 31), sifted, text, old("o_known_id", 42), old("o_known_url", 43))
	f.api.marks = marks
	f.exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES ('kk1','u1','karakeep','karakeep','Karakeep')`)
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, karakeep_id, batch_date, created_at, updated_at)
		VALUES ('i1','u1','kk1','o_known_id','article','https://example.com/other','t','light','o_known_id','2026-09-01','c','c')`)
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, batch_date, created_at, updated_at)
		VALUES ('i2','u1','srcM','x','article','https://EXAMPLE.com/o_known_url/?utm_source=a','t','light','2026-09-01','c','c')`)
	res := f.run("u1")
	got := f.cache("u1")
	if fmt.Sprint(got) != "[o_plain1 o_plain2]" || res.Cached != 2 {
		t.Fatalf("cache = %v, result %+v", got, res)
	}
	var created string
	f.st.DB().QueryRow(`SELECT bookmark_created_at FROM old_bookmark_cache WHERE bookmark_id = 'o_plain1'`).Scan(&created)
	if created != now.Add(-30*24*time.Hour).Format(time.RFC3339) {
		t.Errorf("bookmark_created_at = %q", created)
	}
}

func TestCrawlReadsTwoLearningPagesAndResumes(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.set("u1", "old_sifting_backlog_cap", "0")
	f.api.marks = oldRange(200)
	if res := f.run("u1"); res.Reads != 2 || len(f.cache("u1")) != 40 || f.setting("u1", "old_sifting_cursor") != "40" {
		t.Fatalf("run 1: %+v, cache %d, cursor %q", res, len(f.cache("u1")), f.setting("u1", "old_sifting_cursor"))
	}
	f.api.cursors = nil
	f.run("u1")
	if len(f.api.cursors) == 0 || f.api.cursors[0] != "40" || len(f.cache("u1")) != 80 {
		t.Fatalf("run 2: cursors %v, cache %d", f.api.cursors, len(f.cache("u1")))
	}
}

func TestRecognizedPagesDoNotUseTheBudget(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.set("u1", "old_sifting_backlog_cap", "0")
	var marks []kk.Bookmark
	for i := 0; i < 60; i++ {
		marks = append(marks, recent(fmt.Sprintf("r%02d", i), i+1))
	}
	f.api.marks = append(marks, oldRange(100)...)
	if res := f.run("u1"); res.Reads != 5 || len(f.cache("u1")) != 40 {
		t.Fatalf("run 1: %+v, cache %d", res, len(f.cache("u1")))
	}
	f.set("u1", "old_sifting_cursor", "")
	if res := f.run("u1"); res.Reads != 7 || len(f.cache("u1")) != 80 {
		t.Fatalf("run 2: %+v, cache %d", res, len(f.cache("u1")))
	}

	g := newFixture(t)
	g.connect("u1")
	g.set("u1", "old_sifting_backlog_cap", "0")
	var all []kk.Bookmark
	for i := 0; i < 300; i++ {
		all = append(all, recent(fmt.Sprintf("r%03d", i), i+1))
	}
	g.api.marks = all
	if res := g.run("u1"); res.Reads != 10 || len(g.cache("u1")) != 0 {
		t.Fatalf("read cap: %+v, cache %d", res, len(g.cache("u1")))
	}
}

func TestPassEndsAtOldestAndRestartsNextDay(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.set("u1", "old_sifting_backlog_cap", "0")
	f.api.marks = oldRange(30)
	res := f.run("u1")
	if !res.Passed || f.setting("u1", "old_sifting_cursor") != "" || f.setting("u1", "old_sifting_walked_on") != today {
		t.Fatalf("pass: %+v, cursor %q, walked_on %q", res, f.setting("u1", "old_sifting_cursor"), f.setting("u1", "old_sifting_walked_on"))
	}
	f.api.cursors = nil
	f.run("u1")
	if len(f.api.cursors) != 0 {
		t.Fatalf("a second run the same day listed: %v", f.api.cursors)
	}
	mid := old("o_new", 25)
	marks := append([]kk.Bookmark{}, f.api.marks[:5]...)
	marks = append(marks, mid)
	f.api.marks = append(marks, f.api.marks[5:]...)
	f.now = f.now.Add(24 * time.Hour)
	f.run("u1")
	if len(f.api.cursors) == 0 || f.api.cursors[0] != "" {
		t.Fatalf("next day did not start at the newest: %v", f.api.cursors)
	}
	found := false
	for _, id := range f.cache("u1") {
		found = found || id == "o_new"
	}
	if !found {
		t.Fatal("the new old bookmark was not cached on the next pass")
	}
}

func TestBrokenCursorRestartsTheWalk(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.set("u1", "old_sifting_backlog_cap", "0")
	f.api.marks = oldRange(100)
	f.api.cursorErr = map[string]error{"bogus": errors.New("HTTP 400")}
	f.exec(`INSERT INTO old_bookmark_cache (user_id, bookmark_id, bookmark_created_at) VALUES ('u1', 'x1', '2020-01-01T00:00:00Z')`)
	f.set("u1", "old_sifting_cursor", "bogus")
	res := f.run("u1")
	if !res.Restarted {
		t.Fatalf("result = %+v", res)
	}
	got := f.cache("u1")
	if len(got) != 41 || got[len(got)-1] != "x1" {
		t.Fatalf("cache = %d rows, %v", len(got), got)
	}
	if c := f.setting("u1", "old_sifting_cursor"); c == "bogus" || c == "" {
		t.Fatalf("cursor = %q", c)
	}
}

func TestListFailureKeepsCursorAndCache(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.set("u1", "old_sifting_backlog_cap", "0")
	f.api.marks = oldRange(100)
	f.run("u1")
	before := f.cache("u1")
	f.api.listErr = errors.New("down")
	if _, err := f.s.Run(context.Background(), "u1"); err == nil {
		t.Fatal("no error")
	}
	if f.setting("u1", "old_sifting_cursor") != "40" || fmt.Sprint(f.cache("u1")) != fmt.Sprint(before) {
		t.Fatalf("cursor %q, cache changed: %v", f.setting("u1", "old_sifting_cursor"), f.cache("u1"))
	}
}

func TestPullTakesTheOldestSeenSoFar(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.marks = oldRange(100)
	f.api.marks[39].Summary = "Karakeep summary of o039"
	res := f.run("u1")
	if res.Cached != 40 || res.Pulled != 5 {
		t.Fatalf("result = %+v", res)
	}
	if got := fmt.Sprint(f.items("u1")); got != "[o035 o036 o037 o038 o039]" {
		t.Fatalf("items = %s", got)
	}
	var src, state, kid, pub, batch, title, by, excerpt string
	err := f.st.DB().QueryRow(`SELECT s.kind, i.state, i.karakeep_id, i.published_at, i.batch_date, i.title, COALESCE(i.byline,''), i.excerpt
		FROM items i JOIN sources s ON s.user_id = i.user_id AND s.id = i.source_id WHERE i.external_id = 'o039'`).Scan(&src, &state, &kid, &pub, &batch, &title, &by, &excerpt)
	if err != nil {
		t.Fatal(err)
	}
	created := now.Add(-59 * 24 * time.Hour)
	if src != "karakeep" || state != "light" || kid != "o039" || pub != created.Format(time.RFC3339) || batch != created.Format("2006-01-02") ||
		title != "Title o039" || by != "Example" || excerpt != "Karakeep summary of o039" {
		t.Fatalf("item = %s %s %s %s %s %q %q %q", src, state, kid, pub, batch, title, by, excerpt)
	}
	var plain string
	f.st.DB().QueryRow(`SELECT excerpt FROM items WHERE external_id = 'o038'`).Scan(&plain)
	if plain != "About o038" {
		t.Errorf("excerpt without a summary = %q", plain)
	}
	if n := f.count(`SELECT COUNT(*) FROM old_bookmark_cache WHERE pulled_on = ?`, today); n != 5 {
		t.Errorf("pulled rows = %d", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM outbox`); n != 0 {
		t.Errorf("outbox rows = %d", n)
	}
}

func TestPulledItemsAreInBacklogNotToday(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.marks = oldRange(30)
	f.run("u1")
	tr := triage.New(f.st.DB())
	backlog, err := tr.Cards(context.Background(), "u1", triage.ListBacklog, today)
	if err != nil {
		t.Fatal(err)
	}
	cards, _ := tr.Cards(context.Background(), "u1", triage.ListToday, today)
	if len(backlog) != 5 || len(cards) != 0 {
		t.Fatalf("backlog %d, today %d", len(backlog), len(cards))
	}
}

func TestPullReplacesFailingCandidates(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.set("u1", "old_sifting_backlog_cap", "0")
	f.api.marks = oldRange(15) // o014 is the oldest
	f.run("u1")                // cache only; the cap keeps it from pulling
	f.set("u1", "old_sifting_backlog_cap", "20")
	f.set("u1", "old_sifting_pulled_on", "")
	f.exec(`UPDATE old_bookmark_cache SET pulled_on = NULL`)
	// c1..c5 are the five oldest, in order o014, o013, o012, o011, o010.
	f.api.getErr = map[string]error{"o014": kk.ErrNotFound}
	f.api.marks[13].Archived = true
	f.api.marks[12].Tags = []string{"sifted"}
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, batch_date, created_at, updated_at)
		VALUES ('i1','u1','srcM','z','article','https://example.com/o011','t','archived','2026-09-01','c','c')`)
	f.api.marks[10].Type = "text"
	res := f.run("u1")
	if got := fmt.Sprint(f.items("u1")); got != "[o005 o006 o007 o008 o009]" || res.Pulled != 5 {
		t.Fatalf("items = %s, result %+v", got, res)
	}
	if res.Dropped != 5 {
		t.Fatalf("dropped = %d", res.Dropped)
	}
	for _, id := range []string{"o010", "o011", "o012", "o013", "o014"} {
		if n := f.count(`SELECT COUNT(*) FROM old_bookmark_cache WHERE bookmark_id = ?`, id); n != 0 {
			t.Errorf("%s is still cached", id)
		}
	}
}

func TestPullStopsAtMaxAttempts(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	for i := 0; i < 30; i++ {
		f.exec(`INSERT INTO old_bookmark_cache (user_id, bookmark_id, bookmark_created_at) VALUES ('u1', ?, ?)`, fmt.Sprintf("c%02d", i), fmt.Sprintf("2020-01-%02dT00:00:00Z", i+1))
	}
	f.api.marks = nil // no listing, and every Get is not found
	res := f.run("u1")
	if len(f.api.gets) != 25 || res.Pulled != 0 {
		t.Fatalf("gets %d, result %+v", len(f.api.gets), res)
	}
	if f.setting("u1", "old_sifting_pulled_on") != "" {
		t.Error("the day was marked")
	}
}

func TestPullErrorLeavesTheDayOpen(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.set("u1", "old_sifting_backlog_cap", "0")
	f.api.marks = oldRange(30)
	f.run("u1") // cache only
	f.set("u1", "old_sifting_backlog_cap", "20")
	f.set("u1", "old_sifting_pulled_on", "")
	f.api.getErr = map[string]error{"o028": errors.New("boom")} // the second oldest
	if _, err := f.s.Run(context.Background(), "u1"); err == nil {
		t.Fatal("no error")
	}
	if n := len(f.items("u1")); n != 1 || f.setting("u1", "old_sifting_pulled_on") != "" {
		t.Fatalf("items %d, pulled_on %q", n, f.setting("u1", "old_sifting_pulled_on"))
	}
	f.api.getErr = nil
	f.run("u1")
	if n := len(f.items("u1")); n != 5 || f.setting("u1", "old_sifting_pulled_on") != today {
		t.Fatalf("items %d, pulled_on %q", n, f.setting("u1", "old_sifting_pulled_on"))
	}
}

func TestBacklogCapMath(t *testing.T) {
	for _, c := range []struct{ backlog, daily, cap, want int }{
		{0, 5, 20, 5}, {18, 5, 20, 2}, {20, 5, 20, 0}, {25, 5, 20, 0}, {8, 5, 10, 2}, {0, 3, 20, 3},
	} {
		t.Run(fmt.Sprintf("%d_%d_%d", c.backlog, c.daily, c.cap), func(t *testing.T) {
			f := newFixture(t)
			f.connect("u1")
			f.set("u1", "old_sifting_daily", strconv.Itoa(c.daily))
			f.set("u1", "old_sifting_backlog_cap", strconv.Itoa(c.cap))
			f.backlogItems(c.backlog)
			f.api.marks = oldRange(30)
			f.api.gets = nil
			res := f.run("u1")
			if res.Pulled != c.want {
				t.Fatalf("pulled %d, want %d", res.Pulled, c.want)
			}
			if c.want == 0 && (len(f.api.gets) != 0 || f.setting("u1", "old_sifting_pulled_on") != today) {
				t.Fatalf("full backlog: gets %d, pulled_on %q", len(f.api.gets), f.setting("u1", "old_sifting_pulled_on"))
			}
		})
	}
}

func TestBacklogCountIgnoresWhatIsNotBacklogWork(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.set("u1", "old_sifting_backlog_cap", "3")
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, batch_date, created_at, updated_at) VALUES
		('t1','u1','srcM','t1','article','https://a.example.com/1','t','light','2026-10-11','c','c'),
		('a1','u1','srcM','a1','article','https://a.example.com/2','t','archived','2026-10-01','c','c'),
		('k1','u1','srcM','k1','article','https://a.example.com/3','t','kept','2026-10-01','c','c'),
		('p1','u1','srcM','p1','article','https://a.example.com/4','t','pending_deep','2026-10-01','c','c')`)
	f.exec(`INSERT INTO digests (id, user_id, source_id, batch_date, state) VALUES ('d1','u1','srcM','2026-10-01','light'), ('d2','u1','srcM','2026-10-11','light')`)
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, batch_date, digest_id, created_at, updated_at)
		VALUES ('c1','u1','srcM','c1','article','https://a.example.com/5','t','light','2026-10-01','d1','c','c')`)
	f.api.marks = oldRange(30)
	// Only the earlier light digest counts (1), so room is 3-1 = 2.
	if res := f.run("u1"); res.Pulled != 2 {
		t.Fatalf("pulled %d, want 2", res.Pulled)
	}
}

func TestOnePullPerDay(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.marks = oldRange(60)
	if res := f.run("u1"); res.Pulled != 5 {
		t.Fatalf("day 1: %+v", res)
	}
	f.exec(`UPDATE items SET state = 'archived'`)
	if res := f.run("u1"); res.Pulled != 0 {
		t.Fatalf("same day again pulled %d", res.Pulled)
	}
	f.now = f.now.Add(24 * time.Hour)
	if res := f.run("u1"); res.Pulled != 5 {
		t.Fatalf("day 2: %+v", res)
	}
}

func TestRaisedAgeLimitDropsTheCandidate(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.api.marks = []kk.Bookmark{old("o_20", 20)}
	f.exec(`INSERT INTO old_bookmark_cache (user_id, bookmark_id, bookmark_created_at) VALUES ('u1', 'o_20', ?)`, now.Add(-20*24*time.Hour).Format(time.RFC3339))
	f.set("u1", "max_age_days", "30")
	res := f.run("u1")
	if res.Pulled != 0 || res.Dropped != 1 || len(f.cache("u1")) != 0 {
		t.Fatalf("result %+v, cache %v", res, f.cache("u1"))
	}
}

func TestUsersAreIsolated(t *testing.T) {
	f := newFixture(t)
	f.connect("u1")
	f.connect("u2")
	f.api.marks = oldRange(30)
	f.set("u2", "old_sifting_daily", "0")
	f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, batch_date, created_at, updated_at)
		VALUES ('u2i','u2','srcM2','x','article','https://example.com/o029','t','light','2026-10-01','c','c')`)
	for i := 0; i < 30; i++ {
		f.exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, batch_date, created_at, updated_at)
			VALUES (?, 'u2', 'srcM2', ?, 'article', ?, 't', 'light', '2026-10-01', 'c', 'c')`, fmt.Sprintf("u2b%d", i), fmt.Sprintf("u2b%d", i), fmt.Sprintf("https://u2.example.com/%d", i))
	}
	if res := f.run("u1"); res.Pulled != 5 {
		t.Fatalf("u1 result %+v", res)
	}
	if n := f.count(`SELECT COUNT(*) FROM old_bookmark_cache WHERE user_id = 'u2'`); n != 0 {
		t.Errorf("u2 cache rows = %d", n)
	}
	if n := len(f.items("u2")); n != 0 {
		t.Errorf("u2 karakeep items = %d", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM old_bookmark_cache WHERE user_id = 'u1'`); n == 0 {
		t.Error("u1 cache empty")
	}
}
