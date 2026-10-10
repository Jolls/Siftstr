package karakeep

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Jolls/Siftstr/internal/dest"
)

type bm struct {
	ID, URL, Text, Note, Summary string
	Archived                     bool
	Tags                         map[string]bool
}

// fake is a small stateful Karakeep: it de-duplicates links by URL as the
// real one does.
type fake struct {
	mu    sync.Mutex
	marks []*bm
	auth  string
	fail  int // fail this many next requests with 500
}

func (f *fake) find(id string) *bm {
	for _, b := range f.marks {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	if f.fail > 0 {
		f.fail--
		http.Error(w, "boom", 500)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	str := func(k string) string { s, _ := body[k].(string); return s }
	reply := func(b *bm) { _ = json.NewEncoder(w).Encode(map[string]any{"id": b.ID, "note": b.Note}) }
	p := r.URL.Path
	switch {
	case r.Method == "POST" && p == "/api/v1/bookmarks":
		if str("type") == "link" {
			for _, b := range f.marks {
				if b.URL == str("url") {
					reply(b)
					return
				}
			}
		}
		b := &bm{ID: "b" + string(rune('1'+len(f.marks))), URL: str("url"), Text: str("text"), Note: str("note"), Tags: map[string]bool{}}
		f.marks = append(f.marks, b)
		w.WriteHeader(201)
		reply(b)
	case r.Method == "GET" && p == "/api/v1/bookmarks/search":
		var out []map[string]any
		for _, b := range f.marks {
			if strings.Contains(b.Note, r.URL.Query().Get("q")) {
				out = append(out, map[string]any{"id": b.ID, "note": b.Note})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": out})
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v1/bookmarks/"):
		b := f.find(strings.TrimPrefix(p, "/api/v1/bookmarks/"))
		if b == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": b.ID, "note": b.Note, "summary": b.Summary})
	case r.Method == "PATCH" && strings.HasPrefix(p, "/api/v1/bookmarks/"):
		b := f.find(strings.TrimPrefix(p, "/api/v1/bookmarks/"))
		if b == nil {
			http.NotFound(w, r)
			return
		}
		b.Summary, _ = body["summary"].(string)
		b.Archived, _ = body["archived"].(bool)
		reply(b)
	case r.Method == "POST" && strings.HasSuffix(p, "/tags"):
		b := f.find(strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/bookmarks/"), "/tags"))
		if b == nil {
			http.NotFound(w, r)
			return
		}
		for _, t := range body["tags"].([]any) {
			b.Tags[t.(map[string]any)["tagName"].(string)] = true
		}
		_, _ = w.Write([]byte(`{"attached":[]}`))
	default:
		http.NotFound(w, r)
	}
}

func setup(t *testing.T) (*fake, *Destination) {
	f := &fake{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, New(srv.URL+"/", "tok", nil)
}

func TestSendLinkIsIdempotent(t *testing.T) {
	f, d := setup(t)
	it := dest.Item{ID: "itm_1", URL: "https://example.com/a", Title: "A", Summary: "deep", Archived: true, Tags: []string{"sifted", "promoted"}}
	for i := 0; i < 3; i++ {
		if err := d.Send(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.marks) != 1 {
		t.Fatalf("%d bookmarks after three sends", len(f.marks))
	}
	b := f.marks[0]
	if b.Summary != "Agent Summary: deep" || !b.Archived || !b.Tags["sifted"] || !b.Tags["promoted"] {
		t.Fatalf("%+v", b)
	}
	if f.auth != "Bearer tok" {
		t.Fatalf("auth %q", f.auth)
	}
}

func TestKeepAfterPromoteUnarchivesTheSameBookmark(t *testing.T) {
	f, d := setup(t)
	it := dest.Item{ID: "itm_1", URL: "https://example.com/a", Summary: "deep", Archived: true, Tags: []string{"sifted"}}
	_ = d.Send(context.Background(), it)
	it.Archived = false
	it.Tags = []string{"sifted", "kept"}
	if err := d.Send(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	if len(f.marks) != 1 || f.marks[0].Archived || !f.marks[0].Tags["kept"] {
		t.Fatalf("%d bookmarks, %+v", len(f.marks), f.marks[0])
	}
}

func TestNoteIsFoundByMarkerBeforeCreating(t *testing.T) {
	f, d := setup(t)
	it := dest.Item{ID: "dig_1", Title: "News", Summary: "digest text", Note: true, Tags: []string{"sifted"}}
	for i := 0; i < 2; i++ {
		if err := d.Send(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.marks) != 1 || f.marks[0].Text != "digest text" || f.marks[0].Note != "siftstr:dig_1" {
		t.Fatalf("%d notes: %+v", len(f.marks), f.marks)
	}
}

func TestServerErrorRetriesAndIsNotPermanent(t *testing.T) {
	f, d := setup(t)
	f.fail = 1
	err := d.Send(context.Background(), dest.Item{ID: "i", URL: "https://example.com/a"})
	if err == nil {
		t.Fatal("want error")
	}
	var p interface{ Permanent() bool }
	if asPermanent(err, &p) {
		t.Fatalf("a 500 must retry: %v", err)
	}
	if err := d.Send(context.Background(), dest.Item{ID: "i", URL: "https://example.com/a"}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingURLIsPermanent(t *testing.T) {
	_, d := setup(t)
	err := d.Send(context.Background(), dest.Item{ID: "i"})
	var p interface{ Permanent() bool }
	if !asPermanent(err, &p) {
		t.Fatalf("got %v", err)
	}
}

func TestSummaryMerge(t *testing.T) {
	cases := []struct{ name, current, want string }{
		{"empty", "", "Agent Summary: new"},
		{"ours is replaced", "Agent Summary: old", "Agent Summary: new"},
		{"someone else's is kept", "Karakeep wrote this.", "Karakeep wrote this.\n\nAgent Summary: new"},
		{"ours after theirs is replaced, theirs kept", "Karakeep wrote this.\n\nAgent Summary: old", "Karakeep wrote this.\n\nAgent Summary: new"},
	}
	for _, c := range cases {
		if got := mergeSummary(c.current, "new"); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestExistingSummaryIsKeptAndSendingAgainDoesNotStack(t *testing.T) {
	f, d := setup(t)
	f.marks = append(f.marks, &bm{ID: "b1", URL: "https://example.com/a", Summary: "My own words.", Tags: map[string]bool{}})
	it := dest.Item{ID: "itm_1", URL: "https://example.com/a", Summary: "deep"}
	for i := 0; i < 3; i++ {
		if err := d.Send(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.marks[0].Summary; got != "My own words.\n\nAgent Summary: deep" {
		t.Fatalf("got %q", got)
	}
	it.Summary = "deeper" // a later keep after promote
	_ = d.Send(context.Background(), it)
	if got := f.marks[0].Summary; got != "My own words.\n\nAgent Summary: deeper" {
		t.Fatalf("got %q", got)
	}
}
