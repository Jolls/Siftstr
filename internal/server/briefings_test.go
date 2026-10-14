package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedBriefing gives a user a summarized run for day, and a file when text is
// not empty.
func (e *env) seedBriefing(t *testing.T, name, day, text string) {
	t.Helper()
	uid := e.userID(t, name)
	e.exec(t, `INSERT INTO runs (id, user_id, started_at, summaries_at, day) VALUES (?, ?, 'n', 'n', ?)`, "r_"+name+day, uid, day)
	if text == "" {
		return
	}
	dir := filepath.Join(e.briefDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, day+".md"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBriefingsRequireSession(t *testing.T) {
	e := setup(t, nil, false)
	for _, p := range []string{"/briefings", "/briefings/2026-10-09", "/briefings/2026-10-09/download"} {
		resp, err := e.client().Get(e.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("anonymous %s = %d %q", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestBriefingsListViewAndDownload(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	e.seedBriefing(t, "alice", "2026-10-08", "# Siftstr briefing 2026-10-08\n\nOld <b>day</b>.\n")
	e.seedBriefing(t, "alice", "2026-10-09", "# Siftstr briefing 2026-10-09\n\nNewer day.\n")
	e.seedBriefing(t, "alice", "2026-10-07", "") // a run, but no file

	code, page := e.get(t, c, "/briefings")
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	newer, older := strings.Index(page, "2026-10-09"), strings.Index(page, "2026-10-08")
	if newer < 0 || older < 0 || newer > older {
		t.Errorf("days missing or not newest first:\n%s", page)
	}
	for _, want := range []string{`href="/briefings/2026-10-09"`, `href="/briefings/2026-10-09/download"`} {
		if !strings.Contains(page, want) {
			t.Errorf("list lacks %s", want)
		}
	}
	if !strings.Contains(page, "2026-10-07") || strings.Contains(page, `href="/briefings/2026-10-07`) {
		t.Error("a day with no file should be listed without links")
	}

	code, page = e.get(t, c, "/briefings/2026-10-08")
	if code != http.StatusOK || !strings.Contains(page, "Old &lt;b&gt;day&lt;/b&gt;.") || strings.Contains(page, "<b>day</b>") {
		t.Errorf("view = %d, text not shown escaped:\n%s", code, page)
	}

	resp, err := c.Get(e.srv.URL + "/briefings/2026-10-08/download")
	if err != nil {
		t.Fatal(err)
	}
	body := e.body(t, resp)
	if resp.StatusCode != http.StatusOK || body != "# Siftstr briefing 2026-10-08\n\nOld <b>day</b>.\n" {
		t.Errorf("download = %d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `attachment; filename="siftstr-2026-10-08.md"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/markdown") {
		t.Errorf("Content-Type = %q", got)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("download lacks X-Content-Type-Options: nosniff")
	}
}

func TestBriefingsBadOrMissingDayIs404(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	e.seedBriefing(t, "alice", "2026-10-07", "")
	for _, p := range []string{"/briefings/2026-10-07", "/briefings/2026-10-07/download", "/briefings/2026-10-01", "/briefings/not-a-day",
		"/briefings/..%2Fbob%2F2026-10-09", "/briefings/2026-13-45/download"} {
		if code, _ := e.get(t, c, p); code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", p, code)
		}
	}
}

func TestBriefingsAreIsolatedBetweenUsers(t *testing.T) {
	e := setup(t, nil, false)
	ac, _ := e.signedIn(t, "alice")
	bc, _ := e.signedIn(t, "bob")
	e.seedBriefing(t, "alice", "2026-10-09", "Alice secret text.\n")

	if _, page := e.get(t, bc, "/briefings"); strings.Contains(page, "2026-10-09") {
		t.Error("bob's list shows alice's day")
	}
	for _, p := range []string{"/briefings/2026-10-09", "/briefings/2026-10-09/download"} {
		if code, body := e.get(t, bc, p); code != http.StatusNotFound || strings.Contains(body, "Alice secret") {
			t.Errorf("bob %s = %d", p, code)
		}
	}
	if code, body := e.get(t, ac, "/briefings/2026-10-09"); code != http.StatusOK || !strings.Contains(body, "Alice secret text.") {
		t.Errorf("alice's own view = %d", code)
	}
}
