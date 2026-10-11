package karakeep

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/dest"
)

type bm struct {
	ID, URL, Text, Note, Summary, Status string
	Archived                             bool
	Created                              time.Time
	Tags                                 map[string]bool
}

// fake is a small stateful Karakeep: it de-duplicates links by URL as the
// real one does, honours the archived flag on create, patches only the fields
// it is given, and lists unarchived bookmarks newest first with paging.
type fake struct {
	mu    sync.Mutex
	marks []*bm
	auth  string
	fail  int      // fail this many next requests with 500
	calls []string // "METHOD path", in order
}

func (f *fake) find(id string) *bm {
	for _, b := range f.marks {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (f *fake) add(b *bm) *bm {
	if b.ID == "" {
		b.ID = "b" + strconv.Itoa(len(f.marks)+1)
	}
	if b.Tags == nil {
		b.Tags = map[string]bool{}
	}
	f.marks = append(f.marks, b)
	return b
}

func (b *bm) json() map[string]any {
	tags := []map[string]any{}
	for t := range b.Tags {
		tags = append(tags, map[string]any{"name": t, "attachedBy": "human"})
	}
	out := map[string]any{"id": b.ID, "archived": b.Archived, "note": b.Note, "summary": b.Summary, "tags": tags,
		"createdAt": b.Created.Format(time.RFC3339), "title": nil}
	if b.Status != "" {
		out["summarizationStatus"] = b.Status
	}
	if b.URL != "" {
		out["content"] = map[string]any{"type": "link", "url": b.URL, "title": "Page " + b.URL, "description": "About " + b.URL}
	} else {
		out["content"] = map[string]any{"type": "text", "text": b.Text}
	}
	return out
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = r.Header.Get("Authorization")
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if f.fail > 0 {
		f.fail--
		http.Error(w, "boom", 500)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	str := func(k string) string { s, _ := body[k].(string); return s }
	reply := func(b *bm) { _ = json.NewEncoder(w).Encode(b.json()) }
	p := r.URL.Path
	switch {
	case r.Method == "POST" && p == "/api/v1/bookmarks":
		archived, _ := body["archived"].(bool)
		if str("type") == "link" {
			for _, b := range f.marks {
				if b.URL == str("url") {
					b.Archived = archived // as the real one does: a missing flag means unarchived
					out := b.json()
					out["alreadyExists"] = true
					_ = json.NewEncoder(w).Encode(out)
					return
				}
			}
		}
		b := f.add(&bm{URL: str("url"), Text: str("text"), Note: str("note"), Archived: archived, Created: time.Now()})
		w.WriteHeader(201)
		reply(b)
	case r.Method == "GET" && p == "/api/v1/bookmarks/search":
		var out []map[string]any
		for _, b := range f.marks {
			if strings.Contains(b.Note, r.URL.Query().Get("q")) {
				out = append(out, b.json())
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": out})
	case r.Method == "GET" && p == "/api/v1/bookmarks/check-url":
		var id any
		for _, b := range f.marks {
			if b.URL == r.URL.Query().Get("url") {
				id = b.ID
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"bookmarkId": id})
	case r.Method == "GET" && p == "/api/v1/bookmarks":
		f.list(w, r)
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v1/bookmarks/"):
		b := f.find(strings.TrimPrefix(p, "/api/v1/bookmarks/"))
		if b == nil {
			http.NotFound(w, r)
			return
		}
		reply(b)
	case r.Method == "PATCH" && strings.HasPrefix(p, "/api/v1/bookmarks/"):
		b := f.find(strings.TrimPrefix(p, "/api/v1/bookmarks/"))
		if b == nil {
			http.NotFound(w, r)
			return
		}
		if v, ok := body["summary"].(string); ok {
			b.Summary = v
		}
		if v, ok := body["archived"].(bool); ok {
			b.Archived = v
		}
		reply(b)
	case r.Method == "DELETE" && strings.HasSuffix(p, "/tags"):
		b := f.find(strings.TrimSuffix(strings.TrimPrefix(p, "/api/v1/bookmarks/"), "/tags"))
		if b == nil {
			http.NotFound(w, r)
			return
		}
		for _, t := range body["tags"].([]any) {
			delete(b.Tags, t.(map[string]any)["tagName"].(string))
		}
		_, _ = w.Write([]byte(`{"detached":[]}`))
	case r.Method == "DELETE" && strings.HasPrefix(p, "/api/v1/bookmarks/"):
		id := strings.TrimPrefix(p, "/api/v1/bookmarks/")
		if f.find(id) == nil {
			http.NotFound(w, r)
			return
		}
		for i, b := range f.marks {
			if b.ID == id {
				f.marks = append(f.marks[:i], f.marks[i+1:]...)
				break
			}
		}
		w.WriteHeader(204)
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

// list answers GET /bookmarks?archived=false: newest first, with an opaque
// cursor that is the offset of the next page.
func (f *fake) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var all []*bm
	for _, b := range f.marks {
		if strconv.FormatBool(b.Archived) == q.Get("archived") {
			all = append(all, b)
		}
	}
	for i := 1; i < len(all); i++ { // newest first, stable
		for j := i; j > 0 && all[j].Created.After(all[j-1].Created); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit == 0 {
		limit = 50
	}
	start := 0
	if c := q.Get("cursor"); c != "" {
		start, _ = strconv.Atoi(c)
	}
	end := min(start+limit, len(all))
	out := []map[string]any{}
	for _, b := range all[start:end] {
		out = append(out, b.json())
	}
	var next any
	if end < len(all) {
		next = strconv.Itoa(end)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"bookmarks": out, "nextCursor": next})
}

func setup(t *testing.T) (*fake, *Destination) {
	t.Helper()
	f := &fake{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, New(srv.URL+"/", "tok", nil)
}

func TestSendLinkIsIdempotentAndWritesNoSummary(t *testing.T) {
	f, d := setup(t)
	it := dest.Item{ID: "itm_1", URL: "https://example.com/a", Title: "A", Summary: "ignored", Archived: true, Tags: []string{"sifted", "promoted"}}
	for i := 0; i < 3; i++ {
		if err := d.Send(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.marks) != 1 {
		t.Fatalf("%d bookmarks after three sends", len(f.marks))
	}
	b := f.marks[0]
	if b.Summary != "" || !b.Archived || !b.Tags["sifted"] || !b.Tags["promoted"] {
		t.Fatalf("%+v: a save must set the flag and tags and leave the summary to Karakeep", b)
	}
	if f.auth != "Bearer tok" {
		t.Fatalf("auth %q", f.auth)
	}
	if d.BookmarkID() != b.ID {
		t.Fatalf("reported bookmark %q, want %q", d.BookmarkID(), b.ID)
	}
}

// A re-save without the archived flag would unarchive an existing bookmark, so
// the flag is sent every time, in both directions (checked against Karakeep).
func TestSaveAlwaysSendsTheArchivedFlag(t *testing.T) {
	f, d := setup(t)
	it := dest.Item{ID: "itm_1", URL: "https://example.com/a", Archived: true, Tags: []string{"sifted"}}
	_ = d.Send(context.Background(), it)
	if err := d.Send(context.Background(), it); err != nil || !f.marks[0].Archived {
		t.Fatalf("a repeat save unarchived the bookmark: %+v, %v", f.marks[0], err)
	}
	it.Archived = false
	it.Tags = []string{"sifted", "kept"}
	if err := d.Send(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	if len(f.marks) != 1 || f.marks[0].Archived || !f.marks[0].Tags["kept"] {
		t.Fatalf("%d bookmarks, %+v", len(f.marks), f.marks[0])
	}
}

func TestUpdateSetsFlagAndTagsOnTheBookmarkWithoutCreating(t *testing.T) {
	f, d := setup(t)
	f.add(&bm{URL: "https://example.com/a", Archived: true, Summary: "Karakeep wrote this"})
	if err := d.Send(context.Background(), dest.Item{ID: "itm_1", Verb: dest.VerbUpdate, BookmarkID: "b1", Tags: []string{"sifted", "kept"}}); err != nil {
		t.Fatal(err)
	}
	b := f.marks[0]
	if len(f.marks) != 1 || b.Archived || !b.Tags["kept"] || b.Summary != "Karakeep wrote this" {
		t.Fatalf("%d bookmarks, %+v", len(f.marks), b)
	}
	if d.BookmarkID() != "b1" {
		t.Fatalf("reported %q", d.BookmarkID())
	}
	for _, c := range f.calls {
		if c == "POST /api/v1/bookmarks" {
			t.Fatalf("update created a bookmark: %v", f.calls)
		}
	}
	// A bookmark deleted meanwhile cannot be fixed by retrying.
	err := d.Send(context.Background(), dest.Item{Verb: dest.VerbUpdate, BookmarkID: "gone"})
	var p interface{ Permanent() bool }
	if !asPermanent(err, &p) {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteRemovesTheBookmarkAndAMissingOneIsFine(t *testing.T) {
	f, d := setup(t)
	f.add(&bm{URL: "https://example.com/a"})
	f.add(&bm{URL: "https://example.com/b"})
	for i := 0; i < 2; i++ { // the second send finds it gone
		if err := d.Send(context.Background(), dest.Item{Verb: dest.VerbDelete, BookmarkID: "b1"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.marks) != 1 || f.marks[0].URL != "https://example.com/b" {
		t.Fatalf("%+v", f.marks)
	}
	if err := d.Send(context.Background(), dest.Item{Verb: dest.VerbDelete}); err == nil {
		t.Fatal("a delete with no bookmark must not pass quietly")
	}
}

func TestSummaryOverwritesWithoutAPrefixOrMerge(t *testing.T) {
	f, d := setup(t)
	f.add(&bm{URL: "https://example.com/a", Archived: true, Summary: "Karakeep wrote this.", Tags: map[string]bool{"x": true}})
	it := dest.Item{ID: "itm_1", Verb: dest.VerbSummary, BookmarkID: "b1", Summary: "Claude deep summary"}
	for i := 0; i < 2; i++ {
		if err := d.Send(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
	b := f.marks[0]
	if b.Summary != "Claude deep summary" || !b.Archived || !b.Tags["x"] {
		t.Fatalf("%+v", b)
	}
	if err := d.Send(context.Background(), dest.Item{Verb: dest.VerbSummary, BookmarkID: "b1"}); err == nil {
		t.Fatal("an empty summary would blank Karakeep's own")
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
	if len(f.marks) != 1 || f.marks[0].Text != "digest text" || f.marks[0].Note != "siftstr:dig_1" || f.marks[0].Summary != "digest text" {
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

func TestClientListsNewestFirstAndPages(t *testing.T) {
	f := &fake{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c := NewClient(srv.URL, "tok", nil)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		f.add(&bm{URL: "https://example.com/" + strconv.Itoa(i), Created: base.Add(time.Duration(i) * time.Hour)})
	}
	f.add(&bm{URL: "https://example.com/archived", Archived: true, Created: base.Add(24 * time.Hour)})

	var got []string
	cursor := ""
	pages := 0
	for {
		p, err := c.ListUnarchived(context.Background(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, b := range p.Bookmarks {
			got = append(got, b.URL)
		}
		if cursor = p.NextCursor; cursor == "" {
			break
		}
	}
	want := []string{"https://example.com/4", "https://example.com/3", "https://example.com/2", "https://example.com/1", "https://example.com/0"}
	if strings.Join(got, ",") != strings.Join(want, ",") || pages != 3 {
		t.Fatalf("got %v in %d pages, want newest first with no archived", got, pages)
	}
}

// The shape below is what Karakeep sent in a check against a real instance.
func TestBookmarkParsesKarakeepsShape(t *testing.T) {
	raw := `{"id":"abc","createdAt":"2026-10-11T16:16:06.000Z","modifiedAt":"2026-10-11T16:16:40.000Z","title":null,"archived":true,
	"favourited":false,"taggingStatus":"success","summarizationStatus":"success","note":null,"summary":"A short summary.","source":"api",
	"userId":"u","tags":[{"id":"t1","name":"sieve","attachedBy":"ai"},{"id":"t2","name":"sifted","attachedBy":"human"}],
	"content":{"type":"link","url":"https://example.com/a","title":"A page","description":null,"publisher":"Example Weekly","crawlStatus":"success"},"assets":[]}`
	var b Bookmark
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	if b.ID != "abc" || !b.Archived || b.Summary != "A short summary." || b.SummaryStatus != SummaryDone || b.Type != "link" ||
		b.URL != "https://example.com/a" || b.PageTitle != "A page" || b.Publisher != "Example Weekly" || b.Description != "" || b.Note != "" || b.Title != "" {
		t.Fatalf("%+v", b)
	}
	if !b.HasTag("sifted") || !b.HasTag("sieve") || b.HasTag("kept") {
		t.Fatalf("tags %v", b.Tags)
	}
	if !b.CreatedAt.Equal(time.Date(2026, 10, 11, 16, 16, 6, 0, time.UTC)) {
		t.Fatalf("created %v", b.CreatedAt)
	}
}

func TestClientGetAndDetachTags(t *testing.T) {
	f, d := setup(t)
	f.add(&bm{URL: "https://example.com/a", Summary: "s", Status: SummaryDone, Tags: map[string]bool{"sifted": true, "promoted": true}})
	c := d.c
	b, err := c.Get(context.Background(), "b1")
	if err != nil || b.Summary != "s" || b.SummaryStatus != SummaryDone {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := c.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := c.DetachTags(context.Background(), "b1", []string{"promoted"}); err != nil {
		t.Fatal(err)
	}
	if f.marks[0].Tags["promoted"] || !f.marks[0].Tags["sifted"] {
		t.Fatalf("tags %v", f.marks[0].Tags)
	}
}

// The bookmark made at ingest happens with no action from the user, so it
// must never touch one the user already has: it is theirs, and archiving it
// would hide it from their list.
func TestCreateLeavesAnExistingBookmarkAlone(t *testing.T) {
	f, d := setup(t)
	f.add(&bm{URL: "https://example.com/mine", Archived: false, Summary: "mine", Tags: map[string]bool{"keepme": true}})
	it := dest.Item{ID: "itm_1", URL: "https://example.com/mine", Verb: dest.VerbCreate, Archived: true}
	if err := d.Send(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	b := f.marks[0]
	if len(f.marks) != 1 || b.Archived || b.Summary != "mine" || !b.Tags["keepme"] || len(b.Tags) != 1 {
		t.Fatalf("an existing bookmark was changed: %+v", b)
	}
	if d.BookmarkID() != "" {
		t.Fatalf("recorded %q as Siftstr's own bookmark", d.BookmarkID())
	}
	for _, c := range f.calls {
		if c == "POST /api/v1/bookmarks" {
			t.Fatalf("a create was sent for a URL Karakeep already has: %v", f.calls)
		}
	}
}

func TestCreateMakesAnArchivedBookmarkWhenThereIsNone(t *testing.T) {
	f, d := setup(t)
	it := dest.Item{ID: "itm_1", URL: "https://example.com/new", Title: "N", Verb: dest.VerbCreate, Archived: true}
	for i := 0; i < 2; i++ { // the second send finds its own bookmark, and leaves it alone
		if err := d.Send(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.marks) != 1 || !f.marks[0].Archived {
		t.Fatalf("%+v", f.marks)
	}
}
