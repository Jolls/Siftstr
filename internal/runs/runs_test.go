package runs

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/store"
)

var update = flag.Bool("update", false, "rewrite golden files")

type fixture struct {
	s   *Service
	st  *store.Store
	now time.Time
	seq int
}

// newFixture creates two users. u1 has an individual source (src1), a digest
// source (src2, carryover drop) and a disabled source (src3).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{st: st, now: time.Date(2026, 10, 9, 6, 0, 0, 0, time.UTC)}
	f.s = &Service{DB: st.DB(), Now: func() time.Time { return f.now }, NewID: func(p string) (string, error) {
		f.seq++
		return fmt.Sprintf("%s%03d", p, f.seq), nil
	}}
	f.exec(t, `INSERT INTO users (id, username, password_hash, created_at) VALUES ('u1','u1','x','n'), ('u2','u2','x','n')`)
	f.exec(t, `INSERT INTO sources (id, user_id, kind, external_id, name, granularity, carryover, prompt_override, enabled) VALUES
		('src1','u1','miniflux_feed','1','Some Blog','individual','carry','Be terse.',1),
		('src2','u1','miniflux_feed','2','r/news','digest','drop',NULL,1),
		('src3','u1','miniflux_feed','3','Muted','individual','carry',NULL,0),
		('src9','u2','miniflux_feed','1','Other Blog','individual','carry',NULL,1)`)
	return f
}

func (f *fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := f.st.DB().Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) item(t *testing.T, id, user, src, state, title string, extra ...string) {
	t.Helper()
	published := "2026-10-08T12:00:00Z"
	if len(extra) > 0 {
		published = extra[0]
	}
	f.exec(t, `INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, excerpt, content, state, published_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'article', ?, ?, ?, ?, ?, ?, 'c', 'c')`,
		id, user, src, id, "https://example.com/"+id, title, "excerpt of "+title, "full "+title, state, published)
}

func (f *fixture) state(t *testing.T, id string) (state string) {
	t.Helper()
	if err := f.st.DB().QueryRow(`SELECT state FROM items WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return
}

func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, _ := json.MarshalIndent(v, "", "  ")
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != string(got) {
		t.Errorf("%s differs; run with -update to accept.\ngot:\n%s", name, got)
	}
}

func TestWorkPackageGolden(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.item(t, "itm_a", "u1", "src1", "pending_light", "Newer post", "2026-10-09T01:00:00Z")
	f.item(t, "itm_b", "u1", "src1", "pending_light", "Older post")
	f.item(t, "itm_d", "u1", "src1", "pending_deep", "Promoted post")
	f.exec(t, `UPDATE items SET light_summary = 'earlier light summary' WHERE id = 'itm_d'`)
	f.item(t, "itm_e", "u1", "src2", "pending_light", "Headline one", "2026-10-08T09:00:00Z")
	f.item(t, "itm_f", "u1", "src2", "pending_light", "Headline two", "2026-10-08T10:00:00Z")
	f.item(t, "itm_m", "u1", "src3", "pending_light", "From a muted source")
	f.item(t, "itm_x", "u2", "src9", "pending_light", "Belongs to another user")
	f.item(t, "itm_done", "u1", "src1", "archived", "Already triaged")

	wp, err := f.s.Create(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "work_package.golden.json", wp)
}

func TestCreateIsRepeatableReusesRunAndDigest(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.item(t, "itm_e", "u1", "src2", "pending_light", "Headline one")
	first, err := f.s.Create(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	f.item(t, "itm_f", "u1", "src2", "pending_light", "Headline two")
	second, err := f.s.Create(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if first.RunID != second.RunID {
		t.Fatal("a repeat call within the reuse window should reuse the open run")
	}
	if len(second.Digests) != 1 || second.Digests[0].ID != first.Digests[0].ID || len(second.Digests[0].Entries) != 2 {
		t.Fatalf("digests = %+v", second.Digests)
	}
}

func TestSubmitMovesItemsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.item(t, "itm_a", "u1", "src1", "pending_light", "A")
	f.item(t, "itm_d", "u1", "src1", "pending_deep", "D")
	f.item(t, "itm_e", "u1", "src2", "pending_light", "E")
	f.item(t, "itm_x", "u2", "src9", "pending_light", "Not yours")
	wp, err := f.s.Create(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	batch := []Summary{
		{ID: "itm_a", Stage: "light", MD: " light A "},
		{ID: "itm_d", Stage: "deep", MD: "deep D"},
		{ID: wp.Digests[0].ID, Stage: "light", MD: "digest text"},
		{ID: "itm_x", Stage: "light", MD: "sneaky"},
		{ID: "itm_a", Stage: "deep", MD: "wrong stage now"},
		{ID: "itm_nope", Stage: "light", MD: "missing"},
		{ID: "itm_a", Stage: "light", MD: "  "},
		{ID: "itm_a", Stage: "weird", MD: "x"},
	}
	res, err := f.s.Submit(ctx, "u1", wp.RunID, batch)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 3 || len(res.Skipped) != 5 {
		t.Fatalf("result = %+v", res)
	}
	golden(t, "submit_result.golden.json", res)
	for id, want := range map[string]string{"itm_a": "light", "itm_d": "deep", "itm_e": "light", "itm_x": "pending_light"} {
		if got := f.state(t, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	var sum, day string
	_ = f.st.DB().QueryRow(`SELECT light_summary, batch_date FROM items WHERE id = 'itm_a'`).Scan(&sum, &day)
	if sum != "light A" || day != "2026-10-09" {
		t.Fatalf("stored %q on %q", sum, day)
	}

	// The same batch again changes nothing and accepts nothing.
	res, err = f.s.Submit(ctx, "u1", wp.RunID, batch[:3])
	if err != nil || res.Accepted != 0 || len(res.Skipped) != 3 {
		t.Fatalf("replay = %+v, %v", res, err)
	}
	var fin *string
	_ = f.st.DB().QueryRow(`SELECT summaries_at FROM runs WHERE id = ?`, wp.RunID).Scan(&fin)
	if fin == nil {
		t.Fatal("run not marked finished")
	}
}

func TestSubmitToAnotherUsersRunIsNotFound(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	wp, _ := f.s.Create(ctx, "u1")
	if _, err := f.s.Submit(ctx, "u2", wp.RunID, nil); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.s.Submit(ctx, "u1", "run_nope", nil); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestCarryover(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for _, id := range []string{"itm_carry", "itm_drop", "itm_today", "itm_deep", "itm_deep_drop"} {
		src, state := "src1", "light"
		switch id {
		case "itm_drop":
			src = "src2"
		case "itm_deep":
			state = "deep"
		case "itm_deep_drop":
			src, state = "src2", "deep"
		}
		f.item(t, id, "u1", src, state, id)
		day := "2026-10-08"
		if id == "itm_today" {
			day = "2026-10-09"
		}
		f.exec(t, `UPDATE items SET batch_date = ? WHERE id = ?`, day, id)
	}
	f.exec(t, `INSERT INTO digests (id, user_id, source_id, batch_date, state) VALUES ('dig_old','u1','src2','2026-10-08','light')`)
	f.exec(t, `UPDATE items SET digest_id = 'dig_old' WHERE id = 'itm_drop'`)

	if _, err := f.s.Create(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"itm_carry": "light", "itm_drop": "expired", "itm_today": "light", "itm_deep": "deep", "itm_deep_drop": "expired"} {
		if got := f.state(t, id); got != want {
			t.Errorf("%s = %s, want %s", id, got, want)
		}
	}
	var ds string
	_ = f.st.DB().QueryRow(`SELECT state FROM digests WHERE id = 'dig_old'`).Scan(&ds)
	if ds != "expired" {
		t.Fatalf("digest = %s", ds)
	}
}

func TestDayFollowsUserTimezone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if err := f.s.SetTimezone(ctx, "u1", "Pacific/Auckland"); err != nil {
		t.Fatal(err)
	}
	f.now = time.Date(2026, 10, 9, 14, 0, 0, 0, time.UTC) // Oct 10, 03:00 in Auckland
	wp, err := f.s.Create(ctx, "u1")
	if err != nil || wp.Day != "2026-10-10" || wp.Timezone != "Pacific/Auckland" {
		t.Fatalf("wp = %+v, %v", wp, err)
	}
	wp, _ = f.s.Create(ctx, "u2")
	if wp.Day != "2026-10-09" {
		t.Fatalf("u2 day = %s; one user's timezone must not affect another's", wp.Day)
	}
	// An unreadable stored zone falls back to UTC instead of failing the run.
	f.exec(t, `UPDATE users SET timezone = 'Not/AZone' WHERE id = 'u2'`)
	if wp, err = f.s.Create(ctx, "u2"); err != nil || wp.Timezone != "UTC" {
		t.Fatalf("fallback = %+v, %v", wp, err)
	}
}

func TestSetTimezoneValidation(t *testing.T) {
	f := newFixture(t)
	for _, bad := range []string{"", "Local", "Mars/Olympus", "  "} {
		if err := f.s.SetTimezone(context.Background(), "u1", bad); !errors.Is(err, ErrBadTimezone) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

func TestPromptsDefaultOverrideAndReset(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	got, _ := f.s.Prompts(ctx, "u1")
	if got != Defaults {
		t.Fatal("expected defaults")
	}
	if err := f.s.SetPrompt(ctx, "u1", PromptLight, " custom light "); err != nil {
		t.Fatal(err)
	}
	got, _ = f.s.Prompts(ctx, "u1")
	if got.Light != "custom light" || got.Deep != Defaults.Deep {
		t.Fatalf("got %+v", got)
	}
	if other, _ := f.s.Prompts(ctx, "u2"); other.Light != Defaults.Light {
		t.Fatal("u1's prompt leaked to u2")
	}
	if err := f.s.SetPrompt(ctx, "u1", PromptLight, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = f.s.Prompts(ctx, "u1"); got.Light != Defaults.Light {
		t.Fatal("empty value should restore the default")
	}
	if err := f.s.SetPrompt(ctx, "u1", "bogus", "x"); err == nil {
		t.Fatal("unknown prompt accepted")
	}
	if err := f.s.SetPrompt(ctx, "u1", PromptDeep, strings.Repeat("a", MaxPromptLen+1)); err == nil {
		t.Fatal("overlong prompt accepted")
	}
}

func TestPackageCapsIndividualItems(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for i := 0; i < MaxItemsPerRun+5; i++ {
		f.item(t, fmt.Sprintf("itm_%04d", i), "u1", "src1", "pending_light", "t")
	}
	wp, err := f.s.Create(ctx, "u1")
	if err != nil || len(wp.Items) != MaxItemsPerRun {
		t.Fatalf("items = %d, %v", len(wp.Items), err)
	}
}

func TestRunRowsAreReusedThenPruned(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	count := func() (n int) {
		_ = f.st.DB().QueryRow(`SELECT COUNT(*) FROM runs WHERE user_id = 'u1'`).Scan(&n)
		return
	}
	for i := 0; i < 5; i++ {
		if _, err := f.s.Create(ctx, "u1"); err != nil {
			t.Fatal(err)
		}
	}
	if count() != 1 {
		t.Fatalf("runs = %d after 5 quick calls, want 1", count())
	}
	f.now = f.now.Add(2 * time.Hour) // past the reuse window
	if _, err := f.s.Create(ctx, "u1"); err != nil || count() != 2 {
		t.Fatalf("runs = %d, %v", count(), err)
	}
	f.now = f.now.Add(8 * 24 * time.Hour) // both old runs are abandoned
	if _, err := f.s.Create(ctx, "u1"); err != nil || count() != 1 {
		t.Fatalf("runs = %d after pruning, %v", count(), err)
	}
	var offered int
	_ = f.st.DB().QueryRow(`SELECT COUNT(*) FROM run_items r LEFT JOIN runs ON runs.id = r.run_id WHERE runs.id IS NULL`).Scan(&offered)
	if offered != 0 {
		t.Fatal("pruned runs left offered rows behind")
	}
}

func TestDigestSummaryCoversOnlyEntriesTheRunSent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.item(t, "itm_e1", "u1", "src2", "pending_light", "Seen by Claude")
	wp, err := f.s.Create(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	// A new entry is ingested and attached after Claude received the package.
	f.item(t, "itm_e2", "u1", "src2", "pending_light", "Arrived later")
	if _, err := f.s.Create(ctx, "u1"); err != nil { // a second Create attaches it
		t.Fatal(err)
	}
	res, err := f.s.Submit(ctx, "u1", wp.RunID, []Summary{{ID: wp.Digests[0].ID, Stage: "light", MD: "digest"}})
	if err != nil || res.Accepted != 1 {
		t.Fatalf("submit = %+v, %v", res, err)
	}
	// The reused run re-offered both entries, so both are covered here...
	if f.state(t, "itm_e1") != "light" {
		t.Fatal("offered entry not summarized")
	}
	// ...but an entry attached after the run's last Create is not.
	f.item(t, "itm_e3", "u1", "src2", "pending_light", "Even later")
	f.exec(t, `UPDATE items SET digest_id = ? WHERE id = 'itm_e3'`, wp.Digests[0].ID)
	f.exec(t, `UPDATE items SET state = 'pending_light' WHERE id = 'itm_e1'`)
	f.exec(t, `UPDATE digests SET state = 'pending_light' WHERE id = ?`, wp.Digests[0].ID)
	if _, err := f.s.Submit(ctx, "u1", wp.RunID, []Summary{{ID: wp.Digests[0].ID, Stage: "light", MD: "again"}}); err != nil {
		t.Fatal(err)
	}
	if f.state(t, "itm_e3") != "pending_light" {
		t.Fatal("an entry the run never sent was marked summarized")
	}
}

func TestDigestIsCappedAndLeftoverEntriesGoToTheNextOne(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	for i := 0; i < MaxDigestEntries+7; i++ {
		f.item(t, fmt.Sprintf("itm_%04d", i), "u1", "src2", "pending_light", "t", fmt.Sprintf("2026-10-08T%02d:%02d:00Z", i/60, i%60))
	}
	wp, err := f.s.Create(ctx, "u1")
	if err != nil || len(wp.Digests) != 1 || len(wp.Digests[0].Entries) != MaxDigestEntries {
		t.Fatalf("entries = %d, %v", len(wp.Digests[0].Entries), err)
	}
	if _, err := f.s.Submit(ctx, "u1", wp.RunID, []Summary{{ID: wp.Digests[0].ID, Stage: "light", MD: "d"}}); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(2 * time.Hour)
	next, err := f.s.Create(ctx, "u1")
	if err != nil || len(next.Digests) != 1 || len(next.Digests[0].Entries) != 7 || next.Digests[0].ID == wp.Digests[0].ID {
		t.Fatalf("next = %+v, %v", next.Digests, err)
	}
}

func TestSummariesLandOnTheRunsDayEvenIfSubmittedLater(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.now = time.Date(2026, 10, 9, 23, 55, 0, 0, time.UTC)
	f.item(t, "itm_a", "u1", "src1", "pending_light", "A")
	wp, _ := f.s.Create(ctx, "u1")
	f.now = time.Date(2026, 10, 10, 0, 5, 0, 0, time.UTC) // after midnight
	if _, err := f.s.Submit(ctx, "u1", wp.RunID, []Summary{{ID: "itm_a", Stage: "light", MD: "x"}}); err != nil {
		t.Fatal(err)
	}
	var day string
	_ = f.st.DB().QueryRow(`SELECT batch_date FROM items WHERE id = 'itm_a'`).Scan(&day)
	if day != wp.Day || day != "2026-10-09" {
		t.Fatalf("batch_date = %s, package day = %s", day, wp.Day)
	}
}

func TestRetryDoesNotOverwriteRunRecord(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.item(t, "itm_a", "u1", "src1", "pending_light", "A")
	wp, _ := f.s.Create(ctx, "u1")
	batch := []Summary{{ID: "itm_a", Stage: "light", MD: "x"}}
	if _, err := f.s.Submit(ctx, "u1", wp.RunID, batch); err != nil {
		t.Fatal(err)
	}
	var first, stats string
	_ = f.st.DB().QueryRow(`SELECT finished_at FROM runs WHERE id = ?`, wp.RunID).Scan(&first)
	f.now = f.now.Add(time.Hour)
	if _, err := f.s.Submit(ctx, "u1", wp.RunID, batch); err != nil { // the retry
		t.Fatal(err)
	}
	var again string
	_ = f.st.DB().QueryRow(`SELECT finished_at, stats FROM runs WHERE id = ?`, wp.RunID).Scan(&again, &stats)
	if again != first {
		t.Fatalf("finished_at changed from %s to %s", first, again)
	}
	if !strings.Contains(stats, `"accepted":1`) || !strings.Contains(stats, `"submissions":2`) {
		t.Fatalf("stats = %s", stats)
	}
}

func TestSavingTheDefaultPromptRestoresFollowingTheDefault(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	if err := f.s.SetPrompt(ctx, "u1", PromptLight, Defaults.Light); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.st.DB().QueryRow(`SELECT COUNT(*) FROM user_settings WHERE user_id = 'u1' AND key = 'prompt_light'`).Scan(&n)
	if n != 0 {
		t.Fatal("a prompt equal to the default was stored as an override")
	}
}
