package briefing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	f.item(t, "p", "u1", "s1", "archived", "2026-10-09", "sum p") // promoted, then archived
	f.exec(t, `UPDATE items SET promoted_at = '2026-10-09T07:00:00Z' WHERE id = 'p'`)
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
	for _, want := range []string{"Title a=", "Title b=deleted", "Title c=promoted", "Title d=kept", "Title p=archived", "News digest="} {
		if !strings.Contains(got, want) {
			t.Errorf("today %q is missing %q", got, want)
		}
	}
	if len(b.Today) != 6 {
		t.Errorf("today has %d entries (%s), want 6; digest children and unsummarized items must not appear", len(b.Today), got)
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

	f.exec(t, `UPDATE items SET state = 'archived', promoted_at = 'x' WHERE id = 'a'`)
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

// An item is listed with the excerpt it was triaged on until it has a summary.
func TestEntriesShowTheExcerptUntilThereIsASummary(t *testing.T) {
	f := newFixture(t)
	f.item(t, "plain", "u1", "s1", "light", "2026-10-09", "")
	f.item(t, "promoted", "u1", "s1", "deep", "2026-10-09", "")
	f.exec(t, `UPDATE items SET excerpt = 'the excerpt'`)
	f.exec(t, `UPDATE items SET deep_summary = 'deep text' WHERE id = 'promoted'`)
	b, err := f.s.Build(context.Background(), "u1", "2026-10-09", false)
	if err != nil {
		t.Fatal(err)
	}
	md := b.Markdown()
	if !strings.Contains(md, "> the excerpt") || !strings.Contains(md, "deep text") {
		t.Fatalf("markdown: %s", md)
	}
	if strings.Count(md, "the excerpt") != 1 {
		t.Errorf("the excerpt should give way to a summary: %s", md)
	}
}

func TestEntriesNameTheBylineWhenThereIsOne(t *testing.T) {
	f := newFixture(t)
	f.item(t, "a", "u1", "s1", "light", "2026-10-09", "")
	f.item(t, "b", "u1", "s1", "light", "2026-10-09", "")
	f.exec(t, `UPDATE items SET byline = 'Armstrong Economics' WHERE id = 'b'`)
	b, err := f.s.Build(context.Background(), "u1", "2026-10-09", false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range b.Today {
		got[e.Title] = e.Source
	}
	if got["Title a"] != "Blog" || got["Title b"] != "Armstrong Economics" {
		t.Fatalf("sources = %v", got)
	}
}

func TestDaysListsOnlyTheUsersSummarizedDaysNewestFirst(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec(t, `INSERT INTO runs (id, user_id, started_at, summaries_at, day) VALUES
		('r1','u1','n','n','2026-10-07'), ('r2','u1','n','n','2026-10-09'), ('r3','u1','n','n','2026-10-09'),
		('r4','u1','n',NULL,'2026-10-10'), ('r5','u1','n','n',''), ('r6','u2','n','n','2026-10-08')`)
	got, err := f.s.Days(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-10-09,2026-10-07"; strings.Join(got, ",") != want {
		t.Errorf("days = %v, want %s", got, want)
	}
	if other, _ := f.s.Days(ctx, "u2"); strings.Join(other, ",") != "2026-10-08" {
		t.Errorf("bob's days = %v", other)
	}
}

func TestFileReturnsTheStoredBytesOfTheUsersOwnDay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	dir := filepath.Join(f.dir, "alice")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2026-10-09.md"), []byte("kept <as> written\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.File(ctx, "u1", "2026-10-09")
	if err != nil || string(got) != "kept <as> written\n" {
		t.Fatalf("file = %q, %v", got, err)
	}
	if _, err := f.s.File(ctx, "u1", "2026-10-08"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a day with no file: %v", err)
	}
	if _, err := f.s.File(ctx, "u2", "2026-10-09"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("bob must not get alice's file: %v", err)
	}
	for _, day := range []string{"../alice/2026-10-09", "2026-10-09/../..", "x", ""} {
		if b, err := f.s.File(ctx, "u1", day); err == nil || len(b) != 0 {
			t.Errorf("File(%q) = %q, %v: want an error", day, b, err)
		}
	}
}

func TestDaysIncludesDaysWithAFileAndNoRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.exec(t, `INSERT INTO runs (id, user_id, started_at, summaries_at, day) VALUES ('r1','u1','n','n','2026-10-07')`)
	f.exec(t, `INSERT INTO briefing_days (user_id, day, written_at) VALUES ('u1','2026-10-09','n'), ('u1','2026-10-07','n'), ('u2','2026-10-08','n')`)
	got, err := f.s.Days(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-10-09,2026-10-07"; strings.Join(got, ",") != want {
		t.Errorf("days = %v, want %s", got, want)
	}
}

// With no Claude run at all, the timer step still writes today's file and
// lists the day.
func TestDailyWritesTodayWithoutARun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.item(t, "a", "u1", "s1", "light", "2026-10-09", "")
	f.item(t, "theirs", "u2", "s9", "light", "2026-10-09", "")
	if err := f.s.Daily(ctx, "u1", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	body, err := f.s.File(ctx, "u1", "2026-10-09")
	if err != nil || !strings.Contains(string(body), "Title a") || strings.Contains(string(body), "Title theirs") {
		t.Fatalf("file = %q, %v", body, err)
	}
	if days, _ := f.s.Days(ctx, "u1"); strings.Join(days, ",") != "2026-10-09" {
		t.Errorf("days = %v", days)
	}
	if days, _ := f.s.Days(ctx, "u2"); len(days) != 0 {
		t.Errorf("bob's days = %v", days)
	}
}

func TestDailyPicksUpNewItemsAndLeavesAnUnchangedFileAlone(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.item(t, "a", "u1", "s1", "light", "2026-10-09", "")
	if err := f.s.Daily(ctx, "u1", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.dir, "alice", "2026-10-09.md")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Daily(ctx, "u1", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); !fi.ModTime().Equal(old) {
		t.Errorf("an unchanged file was rewritten")
	}
	f.item(t, "b", "u1", "s1", "light", "2026-10-09", "")
	if err := f.s.Daily(ctx, "u1", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if body, _ := os.ReadFile(p); !strings.Contains(string(body), "Title b") {
		t.Errorf("new item missing:\n%s", body)
	}
}

func TestDailyWritesNothingForAnEmptyDay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.s.Daily(ctx, "u1", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	if days, _ := f.s.Days(ctx, "u1"); len(days) != 0 {
		t.Errorf("days = %v", days)
	}
	if _, err := f.s.File(ctx, "u1", "2026-10-09"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file: %v", err)
	}
}

// Yesterday is left for the morning run, which closes it after carryover.
// The timer closes a day only once it is two days old.
func TestDailyClosesDaysBeforeYesterdayOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.item(t, "a", "u1", "s1", "light", "2026-10-07", "")
	f.item(t, "b", "u1", "s1", "light", "2026-10-08", "")
	f.exec(t, `INSERT INTO briefing_days (user_id, day, written_at) VALUES ('u1','2026-10-07','n'), ('u1','2026-10-08','n')`)
	if err := f.s.Daily(ctx, "u1", "2026-10-09"); err != nil {
		t.Fatal(err)
	}
	old, err := f.s.File(ctx, "u1", "2026-10-07")
	if err != nil || !strings.Contains(string(old), "final: true") {
		t.Errorf("2026-10-07 not closed: %q, %v", old, err)
	}
	if _, err := f.s.File(ctx, "u1", "2026-10-08"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("yesterday was written by the timer: %v", err)
	}
	if last, _ := f.s.finalizedDay(ctx, "u1"); last != "2026-10-07" {
		t.Errorf("marker = %q", last)
	}
}

// kkConns gives u1 and u2 their own Karakeep connection (u1's base URL has a
// trailing slash on purpose).
func (f *fixture) kkConns(t *testing.T) {
	t.Helper()
	f.exec(t, `INSERT INTO connections (id, user_id, kind, base_url) VALUES
		('c1','u1','karakeep','https://kk.example.com/'), ('c2','u2','karakeep','https://kk2.example.com')`)
}

func TestBuildWithoutKarakeepConnectionKeepsOriginalURL(t *testing.T) {
	f := newFixture(t)
	f.item(t, "a", "u1", "s1", "light", "2026-10-09", "sum a")
	f.exec(t, `UPDATE items SET karakeep_id = 'bm_a' WHERE id = 'a'`)
	b, err := f.s.Build(context.Background(), "u1", "2026-10-09", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Today) != 1 || b.Today[0].URL != "https://example.com/a" {
		t.Fatalf("today = %+v", b.Today)
	}
	md := b.Markdown()
	if !strings.Contains(md, "### [Title a](https://example.com/a)") || strings.Contains(md, "Original") {
		t.Errorf("markdown = %s", md)
	}
}

func TestBuildEntriesCarryTheKarakeepBookmarkURL(t *testing.T) {
	f := newFixture(t)
	f.kkConns(t)
	f.item(t, "a", "u1", "s1", "light", "2026-10-09", "sum a")
	f.item(t, "b", "u1", "s1", "light", "2026-10-09", "sum b")
	f.item(t, "old", "u1", "s1", "light", "2026-10-08", "old")
	f.item(t, "o", "u2", "s9", "light", "2026-10-09", "other")
	f.exec(t, `INSERT INTO digests (id, user_id, source_id, batch_date, summary, state) VALUES ('dig1','u1','s3','2026-10-09','digest sum','light')`)
	f.exec(t, `UPDATE items SET karakeep_id = 'bm_' || id WHERE id IN ('a', 'old', 'o')`)

	b, err := f.s.Build(context.Background(), "u1", "2026-10-09", false)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Entry{}
	for _, e := range append(append([]Entry{}, b.Today...), b.Backlog...) {
		byID[e.ID] = e
	}
	if got := byID["a"].KarakeepURL; got != "https://kk.example.com/dashboard/preview/bm_a" {
		t.Errorf("a: KarakeepURL = %q", got)
	}
	if byID["a"].URL != "https://example.com/a" {
		t.Errorf("a: URL = %q, want the original", byID["a"].URL)
	}
	if got := byID["b"].KarakeepURL; got != "" {
		t.Errorf("b has no bookmark but KarakeepURL = %q", got)
	}
	if got := byID["old"].KarakeepURL; got != "https://kk.example.com/dashboard/preview/bm_old" {
		t.Errorf("backlog: KarakeepURL = %q", got)
	}
	if got := byID["dig1"].KarakeepURL; got != "" {
		t.Errorf("a digest has no bookmark but KarakeepURL = %q", got)
	}
	b2, err := f.s.Build(context.Background(), "u2", "2026-10-09", false)
	if err != nil || len(b2.Today) != 1 || b2.Today[0].KarakeepURL != "https://kk2.example.com/dashboard/preview/bm_o" {
		t.Errorf("u2 = %+v, %v; want u2's own base URL", b2.Today, err)
	}
}

func TestMarkdownEntryLinksKarakeepWithOriginalSecondary(t *testing.T) {
	b := Briefing{Day: "2026-10-09", Today: []Entry{{
		Title: "T", URL: "https://example.com/a", KarakeepURL: "https://kk.example.com/dashboard/preview/bm_a",
		Source: "Blog", MediaType: "article", Outcome: "kept",
	}}}
	md := b.Markdown()
	for _, want := range []string{
		"### [T](https://kk.example.com/dashboard/preview/bm_a)\n",
		"\nBlog · article · **kept** · [Original](https://example.com/a)\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
}

func TestMarkdownEntryWithoutBookmarkHasNoOriginalLink(t *testing.T) {
	b := Briefing{Day: "2026-10-09", Today: []Entry{{Title: "T", URL: "https://example.com/a", Source: "Blog", MediaType: "article"}}}
	md := b.Markdown()
	if !strings.Contains(md, "### [T](https://example.com/a)\n\nBlog · article\n") || strings.Contains(md, "Original") {
		t.Errorf("markdown = %s", md)
	}
}
