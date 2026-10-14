package briefing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkdownPutsTheHighlightFirstUnderToday(t *testing.T) {
	b := Briefing{
		Day: "2026-10-09",
		Highlight: &Highlight{
			Text: "A passage\nover two lines.", Note: "Why I marked it", Color: "yellow",
			Title: "An [odd] page", URL: "https://example.com/a_(1)", Byline: "example.com", KarakeepURL: "https://kk.example.com/dashboard/preview/b1",
		},
		Today: []Entry{{Title: "Entry", Source: "Blog", MediaType: "article"}},
	}
	want := `---
date: 2026-10-09
today: 1
backlog: 0
final: false
---

# Siftstr briefing 2026-10-09

## Today

### Highlight

> A passage
> over two lines.

Note: Why I marked it

From [An \[odd\] page](https://example.com/a_%281%29) · example.com · [Karakeep](https://kk.example.com/dashboard/preview/b1)

### Entry

Blog · article

## Backlog

Nothing carried over.
`
	if got := b.Markdown(); got != want {
		t.Errorf("markdown mismatch:\n%s\nwant:\n%s", got, want)
	}
}

func TestMarkdownHighlightWithoutNoteOrBookmark(t *testing.T) {
	b := Briefing{Day: "2026-10-09", Highlight: &Highlight{Text: "Just the text."}}
	md := b.Markdown()
	if !strings.Contains(md, "## Today\n\n### Highlight\n\n> Just the text.\n\nNothing arrived today.\n") {
		t.Errorf("markdown = %s", md)
	}
	if strings.Contains(md, "Note:") || strings.Contains(md, "From") {
		t.Error("an absent note and bookmark should leave no empty lines behind")
	}
}

func TestMarkdownWithoutAHighlightIsUnchanged(t *testing.T) {
	if strings.Contains((Briefing{Day: "2026-10-09"}).Markdown(), "Highlight") {
		t.Error("a day with no highlight must not mention one")
	}
}

func TestBuildReadsOnlyThatUsersHighlightForThatDay(t *testing.T) {
	f := newFixture(t)
	f.exec(t, `INSERT INTO highlight_picks (user_id, day, highlight_id, text, note, color, bookmark_title, bookmark_url, byline, created_at) VALUES
		('u1', '2026-10-09', 'h1', 'alice text', 'alice note', 'red', 'Alice page', 'https://example.com/a', 'example.com', 'c'),
		('u2', '2026-10-09', 'h2', 'bob text', '', '', '', '', '', 'c'),
		('u1', '2026-10-08', 'h3', 'alice yesterday', '', '', '', '', '', 'c')`)
	b, err := f.s.Build(context.Background(), "u1", "2026-10-09", false)
	if err != nil {
		t.Fatal(err)
	}
	if b.Highlight == nil || b.Highlight.Text != "alice text" || b.Highlight.Note != "alice note" || b.Highlight.URL != "https://example.com/a" {
		t.Fatalf("highlight = %+v", b.Highlight)
	}
	if strings.Contains(b.Markdown(), "bob text") {
		t.Error("another user's highlight leaked into the markdown")
	}
	none, err := f.s.Build(context.Background(), "u1", "2026-10-10", false)
	if err != nil || none.Highlight != nil {
		t.Errorf("a day with no pick = %+v, %v", none.Highlight, err)
	}
}

func TestRewritingADayKeepsItsHighlight(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec(t, `INSERT INTO highlight_picks (user_id, day, highlight_id, text, created_at) VALUES ('u1', '2026-10-09', 'h1', 'kept text', 'c')`)
	for _, final := range []bool{false, true} {
		if err := f.s.Write(ctx, "u1", "2026-10-09", final); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(f.dir, "alice", "2026-10-09.md"))
		if err != nil || !strings.Contains(string(got), "> kept text") {
			t.Fatalf("final=%v: file = %v %q", final, err, got)
		}
	}
}
