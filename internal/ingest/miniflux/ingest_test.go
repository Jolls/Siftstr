package miniflux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/connections"
	"github.com/Jolls/Siftstr/internal/store"
)

type fake struct {
	feeds   []Feed
	entries []Entry
	gotURL  string
	gotTok  string
}

func (f *fake) Feeds(context.Context) ([]Feed, error)          { return f.feeds, nil }
func (f *fake) UnreadEntries(context.Context) ([]Entry, error) { return f.entries, nil }

func setup(t *testing.T) (*Ingester, *fake, *store.Store) {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, u := range []string{"u1", "u2"} {
		if _, err := st.DB().Exec(`INSERT INTO users (id, username, password_hash, created_at) VALUES (?, ?, 'x', 'now')`, u, u); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := connections.New(st.DB(), bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	g := &Ingester{
		DB: st.DB(), Conns: cs,
		New: func(baseURL, token string) API { f.gotURL, f.gotTok = baseURL, token; return f },
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	return g, f, st
}

func connect(t *testing.T, g *Ingester, user, token string) {
	t.Helper()
	if _, err := g.Conns.Create(context.Background(), user, connections.Input{Kind: "miniflux", BaseURL: "https://mf.example.com", Secret: &token}); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunMirrorsSourcesAndPullsItemsOnce(t *testing.T) {
	ctx := context.Background()
	g, f, st := setup(t)
	connect(t, g, "u1", "tok1")
	f.feeds = []Feed{{ID: 1, Title: "Blog", Category: "News"}, {ID: 2, Title: "Chan", Category: "Video"}}
	f.entries = []Entry{
		{ID: 10, FeedID: 1, Title: " Hello ", URL: "https://blog.example/a", Content: "<p>Hi <b>there</b></p>"},
		{ID: 11, FeedID: 2, Title: "Vid", URL: "https://www.youtube.com/watch?v=x"},
		{ID: 12, FeedID: 99, Title: "orphan", URL: "https://x.example"},
	}
	res, err := g.Run(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Sources != 2 || res.Items != 2 {
		t.Fatalf("result = %+v", res)
	}
	if f.gotURL != "https://mf.example.com" || f.gotTok != "tok1" {
		t.Fatalf("factory got %q %q", f.gotURL, f.gotTok)
	}
	var title, excerpt, state, mt string
	if err := st.DB().QueryRow(`SELECT title, excerpt, state, media_type FROM items WHERE external_id = '10'`).Scan(&title, &excerpt, &state, &mt); err != nil {
		t.Fatal(err)
	}
	if title != "Hello" || excerpt != "Hi there" || state != "pending_light" || mt != "article" {
		t.Fatalf("item = %q %q %q %q", title, excerpt, state, mt)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items WHERE external_id = '11' AND media_type = 'video'`); got != 1 {
		t.Fatal("video not derived")
	}

	res, err = g.Run(ctx, "u1") // second run: dedupe by entry ID
	if err != nil || res.Items != 0 {
		t.Fatalf("rerun = %+v, %v", res, err)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items`); got != 2 {
		t.Fatalf("items = %d", got)
	}
}

func TestRunKeepsUserSourceSettingsAndSkipsDisabled(t *testing.T) {
	ctx := context.Background()
	g, f, st := setup(t)
	connect(t, g, "u1", "t")
	f.feeds = []Feed{{ID: 1, Title: "Old", Category: "A"}}
	if _, err := g.Run(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE sources SET granularity = 'digest', max_depth = 'light', enabled = 0`); err != nil {
		t.Fatal(err)
	}
	f.feeds = []Feed{{ID: 1, Title: "New", Category: "B"}}
	f.entries = []Entry{{ID: 5, FeedID: 1, Title: "t", URL: "https://x.example"}}
	res, err := g.Run(ctx, "u1")
	if err != nil || res.Items != 0 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	var name, cat, gran, depth string
	if err := st.DB().QueryRow(`SELECT name, category, granularity, max_depth FROM sources`).Scan(&name, &cat, &gran, &depth); err != nil {
		t.Fatal(err)
	}
	if name != "New" || cat != "B" || gran != "digest" || depth != "light" {
		t.Fatalf("source = %q %q %q %q", name, cat, gran, depth)
	}
}

func TestRunIsScopedToOneUser(t *testing.T) {
	ctx := context.Background()
	g, f, st := setup(t)
	connect(t, g, "u1", "t1")
	connect(t, g, "u2", "t2")
	f.feeds = []Feed{{ID: 1, Title: "Same feed id"}}
	f.entries = []Entry{{ID: 1, FeedID: 1, Title: "t", URL: "https://x.example"}}
	if _, err := g.Run(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items WHERE user_id = 'u2'`); got != 0 {
		t.Fatal("u2 got u1's items")
	}
	if _, err := g.Run(ctx, "u2"); err != nil {
		t.Fatal(err)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items`); got != 2 {
		t.Fatalf("items = %d, want one per user", got)
	}
}

func TestRunWithoutConnection(t *testing.T) {
	g, _, _ := setup(t)
	if _, err := g.Run(context.Background(), "u1"); !errors.Is(err, ErrNoConnection) {
		t.Fatalf("err = %v", err)
	}
}

func TestExcerptLengthSetting(t *testing.T) {
	ctx := context.Background()
	g, f, st := setup(t)
	connect(t, g, "u1", "t")
	if _, err := st.DB().Exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'excerpt_length', '5')`); err != nil {
		t.Fatal(err)
	}
	f.feeds = []Feed{{ID: 1, Title: "B"}}
	f.entries = []Entry{{ID: 1, FeedID: 1, Title: "t", URL: "https://x.example", Content: "abcdefghij"}}
	if _, err := g.Run(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	var ex string
	if err := st.DB().QueryRow(`SELECT excerpt FROM items`).Scan(&ex); err != nil || ex != "abcde…" {
		t.Fatalf("excerpt = %q, %v", ex, err)
	}
}

func TestMediaType(t *testing.T) {
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{URL: "https://blog.example/p"}, "article"},
		{Entry{URL: "https://youtu.be/x"}, "video"},
		{Entry{URL: "https://m.youtube.com/watch?v=x"}, "video"},
		{Entry{URL: "https://notyoutube.com/x"}, "article"},
		{Entry{URL: "https://old.reddit.com/r/x"}, "post"},
		{Entry{URL: "https://show.example/ep", Enclosures: []Enclosure{{MimeType: "audio/mpeg"}}}, "podcast"},
		{Entry{URL: "https://show.example/ep", Enclosures: []Enclosure{{MimeType: "video/mp4"}}}, "video"},
		{Entry{URL: "::bad"}, "article"},
	}
	for _, c := range cases {
		if got := MediaType(c.e); got != c.want {
			t.Errorf("%s %v = %s, want %s", c.e.URL, c.e.Enclosures, got, c.want)
		}
	}
}

func TestExcerpt(t *testing.T) {
	if got := Excerpt("<script>x()</script><p>A &amp; B</p>\n<p>C</p>", 100); got != "A & B C" {
		t.Fatalf("got %q", got)
	}
	if got := Excerpt("héllo wörld", 5); got != "héllo…" {
		t.Fatalf("got %q", got)
	}
}

func TestClientAgainstHTTPFake(t *testing.T) {
	mux := http.NewServeMux()
	checkToken := func(r *http.Request) {
		if r.Header.Get("X-Auth-Token") != "tok" {
			t.Errorf("missing token on %s", r.URL.Path)
		}
	}
	mux.HandleFunc("/v1/feeds", func(w http.ResponseWriter, r *http.Request) {
		checkToken(r)
		_, _ = w.Write([]byte(`[{"id":3,"title":"F","category":{"title":"Cat"}}]`))
	})
	mux.HandleFunc("/v1/entries", func(w http.ResponseWriter, r *http.Request) {
		checkToken(r)
		q := r.URL.Query()
		if q.Get("status") != "unread" || q.Get("direction") != "asc" {
			t.Errorf("query = %v", q)
		}
		var entries []map[string]any
		switch q.Get("offset") {
		case "0":
			for i := 0; i < pageSize; i++ {
				entries = append(entries, map[string]any{"id": i + 1, "feed_id": 3, "title": "t", "url": "u", "published_at": "2026-01-01T10:00:00+01:00"})
			}
		case "100":
			entries = append(entries, map[string]any{"id": 101, "feed_id": 3, "title": "last", "url": "u",
				"enclosures": []map[string]string{{"url": "e", "mime_type": "audio/mpeg"}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total": 101, "entries": entries})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL+"/", "tok", nil)
	feeds, err := c.Feeds(context.Background())
	if err != nil || len(feeds) != 1 || feeds[0].Category != "Cat" {
		t.Fatalf("feeds = %+v, %v", feeds, err)
	}
	es, err := c.UnreadEntries(context.Background())
	if err != nil || len(es) != 101 || !es[0].Published.Equal(time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)) || es[100].Enclosures[0].MimeType != "audio/mpeg" {
		t.Fatalf("entries = %d, %v", len(es), err)
	}
}

func TestClientErrorDoesNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 401) }))
	defer srv.Close()
	_, err := NewClient(srv.URL, "supersecret", nil).Feeds(context.Background())
	if err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("err = %v", err)
	}
}

func TestMaxAgeSkipsOldEntriesAndStoresPublishedDate(t *testing.T) {
	ctx := context.Background()
	g, f, st := setup(t) // now = 2026-01-02
	connect(t, g, "u1", "t")
	f.feeds = []Feed{{ID: 1, Title: "B"}}
	f.entries = []Entry{
		{ID: 1, FeedID: 1, Title: "fresh", URL: "https://x.example/1", Published: time.Date(2025, 12, 30, 8, 0, 0, 0, time.UTC)},
		{ID: 2, FeedID: 1, Title: "stale", URL: "https://x.example/2", Published: time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC)},
		{ID: 3, FeedID: 1, Title: "undated", URL: "https://x.example/3"},
	}
	res, err := g.Run(ctx, "u1")
	if err != nil || res.Items != 2 || res.TooOld != 1 {
		t.Fatalf("res = %+v, %v", res, err)
	}
	var pub string
	if err := st.DB().QueryRow(`SELECT published_at FROM items WHERE external_id = '1'`).Scan(&pub); err != nil || pub != "2025-12-30T08:00:00Z" {
		t.Fatalf("published_at = %q, %v", pub, err)
	}
	if got := count(t, st, `SELECT COUNT(*) FROM items WHERE external_id = '3' AND published_at IS NULL`); got != 1 {
		t.Fatal("undated entry should be kept with a null date")
	}

	// 0 switches the limit off; a custom value moves it.
	if _, err := st.DB().Exec(`INSERT INTO user_settings (user_id, key, value) VALUES ('u1', 'max_age_days', '0')`); err != nil {
		t.Fatal(err)
	}
	if res, err = g.Run(ctx, "u1"); err != nil || res.Items != 1 || res.TooOld != 0 {
		t.Fatalf("no limit: %+v, %v", res, err)
	}
}
