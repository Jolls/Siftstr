package briefing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Jolls/Siftstr/internal/store"
)

type fixture struct {
	s   *Service
	st  *store.Store
	dir string
}

// newFixture has two users, each with one source. u1 is "alice".
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{st: st, dir: t.TempDir()}
	f.s = New(st.DB(), f.dir)
	f.exec(t, `INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','alice','x','n'), ('u2','bob','x','n')`)
	f.exec(t, `INSERT INTO sources (id, user_id, kind, external_id, name, granularity) VALUES
		('s1','u1','miniflux_feed','1','Blog','individual'),
		('s3','u1','miniflux_feed','3','News','digest'),
		('s9','u2','miniflux_feed','1','Other','individual')`)
	return f
}

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.DB().Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) item(t *testing.T, id, user, src, state, batch, summary string) {
	t.Helper()
	f.exec(t, `INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, light_summary, batch_date, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'article', 'https://example.com/'||?, 'Title '||?, ?, ?, ?, 'c', 'c')`, id, user, src, id, id, id, state, summary, batch)
}

func titles(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Title+"="+e.Outcome)
	}
	return out
}

func TestBuildSplitsTodayAndBacklog(t *testing.T) {
	f := newFixture(t)
	f.item(t, "a", "u1", "s1", "light", "2026-10-09", "sum a")
	f.item(t, "b", "u1", "s1", "archived", "2026-10-09", "sum b")
	f.item(t, "c", "u1", "s1", "pending_deep", "2026-10-09", "sum c")
	f.item(t, "d", "u1", "s1", "kept", "2026-10-09", "sum d")
	f.item(t, "old", "u1", "s1", "light", "2026-10-08", "old sum")
	f.item(t, "olddone", "u1", "s1", "archived", "2026-10-08", "x") // not waiting: not in backlog
	f.item(t, "wait", "u1", "s1", "pending_light", "", "")          // not summarized yet
	f.exec(t, `INSERT INTO digests (id, user_id, source_id, batch_date, summary, state) VALUES ('dig1','u1','s3','2026-10-09','digest sum','light')`)
	f.item(t, "child", "u1", "s3", "light", "2026-10-09", "child sum")
	f.exec(t, `UPDATE items SET digest_id = 'dig1' WHERE id = 'child'`)

	b, err := f.s.Build(context.Background(), "u1", "2026-10-09", false)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(titles(b.Today), ",")
	for _, want := range []string{"Title a=", "Title b=archived", "Title c=promoted", "Title d=kept", "News digest="} {
		if !strings.Contains(got, want) {
			t.Errorf("today %q is missing %q", got, want)
		}
	}
	if len(b.Today) != 5 {
		t.Errorf("today has %d entries (%s), want 5; digest children and unsummarized items must not appear", len(b.Today), got)
	}
	if len(b.Backlog) != 1 || b.Backlog[0].Title != "Title old" || b.Backlog[0].Outcome != "" {
		t.Errorf("backlog = %v", titles(b.Backlog))
	}
}

func TestFinalOutcomes(t *testing.T) {
	f := newFixture(t)
	f.item(t, "a", "u1", "s1", "light", "2026-10-08", "s")
	f.item(t, "e", "u1", "s1", "expired", "2026-10-08", "s")
	f.item(t, "k", "u1", "s1", "kept", "2026-10-08", "s")
	b, err := f.s.Build(context.Background(), "u1", "2026-10-08", true)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, e := range b.Today {
		m[e.Title] = e.Outcome
	}
	if m["Title a"] != "carried over" || m["Title e"] != "expired" || m["Title k"] != "kept" {
		t.Errorf("outcomes = %v", m)
	}
}

func TestBuildIsolatesUsers(t *testing.T) {
	f := newFixture(t)
	f.item(t, "mine", "u1", "s1", "light", "2026-10-09", "mine")
	f.item(t, "theirs", "u2", "s9", "light", "2026-10-09", "theirs")
	f.item(t, "theirsold", "u2", "s9", "light", "2026-10-01", "theirs")
	b, err := f.s.Build(context.Background(), "u1", "2026-10-09", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Today) != 1 || b.Today[0].ID != "mine" || len(b.Backlog) != 0 {
		t.Errorf("u1 saw today=%v backlog=%v", titles(b.Today), titles(b.Backlog))
	}
	md := b.Markdown()
	if strings.Contains(md, "theirs") {
		t.Error("another user's item leaked into the markdown")
	}
}

func TestMarkdownLayout(t *testing.T) {
	b := Briefing{Day: "2026-10-09", Today: []Entry{
		{Title: "A [odd]\ntitle", URL: "https://example.com/a_(1)", Source: "Blog", MediaType: "article", Summary: "The summary.", Outcome: "kept"},
	}}
	want := `---
date: 2026-10-09
today: 1
backlog: 0
final: false
---

# Siftstr briefing 2026-10-09

## Today

### [A \[odd\] title](https://example.com/a_%281%29)

Blog · article · **kept**

The summary.

## Backlog

Nothing carried over.
`
	if got := b.Markdown(); got != want {
		t.Errorf("markdown mismatch:\n%s\nwant:\n%s", got, want)
	}
}

func TestWriteAndFinalize(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.item(t, "a", "u1", "s1", "light", "2026-10-08", "s")
	f.item(t, "z", "u2", "s9", "light", "2026-10-08", "s")
	f.exec(t, `INSERT INTO runs (id, user_id, started_at, summaries_at, day) VALUES ('r1','u1','n','n','2026-10-08'), ('r2','u2','n','n','2026-10-08')`)

	if err := f.s.Write(ctx, "u1", "2026-10-08", false); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.dir, "alice", "2026-10-08.md")
	body, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(body), "final: false") {
		t.Fatalf("live file: %v %q", err, body)
	}

	f.exec(t, `UPDATE items SET state = 'archived' WHERE id = 'a'`)
	if err := f.s.Finalize(ctx, "u1", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(p)
	if !strings.Contains(string(body), "final: true") || !strings.Contains(string(body), "**archived**") {
		t.Errorf("file was not closed with outcomes:\n%s", body)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "bob")); err == nil {
		t.Error("finalizing alice wrote bob's folder")
	}

	// A second pass has nothing left to close: the file is not touched.
	if err := os.WriteFile(p, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finalize(ctx, "u1", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if body, _ = os.ReadFile(p); string(body) != "edited" {
		t.Error("an already closed day was rewritten")
	}
	// The current day is never closed early.
	if err := f.s.Finalize(ctx, "u1", "2026-10-08"); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRefusesUnsafeUsername(t *testing.T) {
	f := newFixture(t)
	f.exec(t, `UPDATE users SET username = '../evil' WHERE id = 'u1'`)
	if err := f.s.Write(context.Background(), "u1", "2026-10-09", false); err == nil {
		t.Fatal("wrote with a path-like username")
	}
	if err := f.s.Write(context.Background(), "u2", "../2026", false); err == nil {
		t.Fatal("wrote with a bad day")
	}
}

func TestLiveWriteDoesNotReopenClosedDay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.item(t, "a", "u1", "s1", "light", "2026-10-08", "s")
	f.exec(t, `INSERT INTO runs (id, user_id, started_at, summaries_at, day) VALUES ('r1','u1','n','n','2026-10-08')`)
	if err := f.s.Finalize(ctx, "u1", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Write(ctx, "u1", "2026-10-08", false); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(filepath.Join(f.dir, "alice", "2026-10-08.md"))
	if !strings.Contains(string(body), "final: true") {
		t.Errorf("closed day was reopened:\n%s", body)
	}
}

func TestFinalizeContinuesPastAFailedDay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.item(t, "a", "u1", "s1", "light", "2026-10-07", "s")
	f.item(t, "b", "u1", "s1", "light", "2026-10-08", "s")
	f.exec(t, `INSERT INTO runs (id, user_id, started_at, summaries_at, day) VALUES ('r1','u1','n','n','2026-10-07'), ('r2','u1','n','n','2026-10-08')`)
	// A directory where the first day's file belongs makes that one write fail.
	if err := os.MkdirAll(filepath.Join(f.dir, "alice", "2026-10-07.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finalize(ctx, "u1", "2026-10-09"); err == nil {
		t.Fatal("expected the failed day to be reported")
	}
	if _, err := os.Stat(filepath.Join(f.dir, "alice", "2026-10-08.md")); err != nil {
		t.Errorf("later day was blocked by the failed one: %v", err)
	}
	if last, _ := f.s.finalizedDay(ctx, "u1"); last != "" {
		t.Errorf("marker moved past a failed day: %q", last)
	}
}

func TestBacklogKeepsPromotedWaitingForDeepSummary(t *testing.T) {
	f := newFixture(t)
	f.item(t, "p", "u1", "s1", "pending_deep", "2026-10-08", "s")
	b, err := f.s.Build(context.Background(), "u1", "2026-10-09", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Backlog) != 1 || b.Backlog[0].Outcome != "promoted" {
		t.Errorf("backlog = %v", titles(b.Backlog))
	}
}
