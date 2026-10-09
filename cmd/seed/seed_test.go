package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/store"
	"github.com/Jolls/Siftstr/internal/triage"
)

func readExample(t *testing.T) File {
	t.Helper()
	raw, err := os.ReadFile("example.json")
	if err != nil {
		t.Fatal(err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(`INSERT INTO users (id, username, password_hash, timezone, created_at) VALUES ('u1', 'alice', 'x', 'UTC', 'n')`); err != nil {
		t.Fatal(err)
	}
	return st
}

// The committed example must stay generic: no real hosts or accounts.
func TestExampleUsesOnlyExampleDomains(t *testing.T) {
	for _, it := range readExample(t).Items {
		if !strings.HasPrefix(it.URL, "https://example.com/") {
			t.Errorf("%q: URL %q is not under example.com", it.Title, it.URL)
		}
	}
}

func TestLoadSplitsTodayAndBacklogAndIsRepeatable(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	f := readExample(t)

	s, i, err := load(ctx, st.DB(), "alice", f, now)
	if err != nil || s != 3 || i != len(f.Items) {
		t.Fatalf("load = %d sources, %d items, %v", s, i, err)
	}
	if s, i, err = load(ctx, st.DB(), "alice", f, now); err != nil || s != 0 || i != 0 {
		t.Fatalf("second load = %d, %d, %v; want nothing added", s, i, err)
	}

	tr := &triage.Service{DB: st.DB(), Now: func() time.Time { return now }}
	today, _ := tr.Cards(ctx, "u1", triage.ListToday, "2026-10-09")
	back, _ := tr.Cards(ctx, "u1", triage.ListBacklog, "2026-10-09")
	if len(today) != 6 || len(back) != 2 {
		t.Fatalf("today %d, backlog %d; want 6 and 2", len(today), len(back))
	}
	var deep, lightOnly int
	for _, c := range today {
		if c.State == "deep" {
			deep++
		}
		if c.Source == "Example Headlines" && !c.CanPromote {
			lightOnly++
		}
	}
	if deep != 1 || lightOnly != 2 {
		t.Fatalf("deep %d, light-only without promote %d", deep, lightOnly)
	}
}

func TestLoadRejectsUnknownUserAndSource(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	now := time.Now()
	if _, _, err := load(ctx, st.DB(), "nobody", readExample(t), now); err == nil {
		t.Fatal("unknown user accepted")
	}
	bad := File{Items: []Item{{Source: "missing", Title: "t", URL: "https://example.com/x"}}}
	if _, _, err := load(ctx, st.DB(), "alice", bad, now); err == nil {
		t.Fatal("unknown source accepted")
	}
}

func TestExportRoundTripsAndLeaksNothingElse(t *testing.T) {
	ctx := context.Background()
	src := newStore(t)
	now := time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)
	if _, _, err := load(ctx, src.DB(), "alice", readExample(t), now); err != nil {
		t.Fatal(err)
	}
	out, err := export(ctx, src.DB(), "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	for _, leak := range []string{"u1", "alice", "itm_", "src_", "seed-"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("export contains %q", leak)
		}
	}

	dst := newStore(t)
	if _, i, err := load(ctx, dst.DB(), "alice", out, now); err != nil || i != len(out.Items) {
		t.Fatalf("reload = %d, %v", i, err)
	}
	var older int
	dst.DB().QueryRow(`SELECT COUNT(*) FROM items WHERE batch_date < '2026-10-09'`).Scan(&older)
	if older != 2 {
		t.Fatalf("days_ago did not survive the round trip: %d older items", older)
	}
}
