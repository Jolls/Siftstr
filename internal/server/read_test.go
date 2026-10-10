package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRequiresSession(t *testing.T) {
	e := setup(t, nil, false)
	resp, err := e.client().Get(e.srv.URL + "/read")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous /read = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// A submitted summary shows on /read for its owner only, carries no
// controls, and is written to the owner's markdown folder only.
func TestSummariesReachReadPageAndFile(t *testing.T) {
	e := setup(t, nil, false)
	_, aliceKey := e.apiUser(t, "alice")
	_, _ = e.apiUser(t, "bob")

	_, body := e.apiCall(t, aliceKey, "POST", "/api/v1/runs", "")
	var wp struct {
		RunID string `json:"run_id"`
		Day   string `json:"day"`
	}
	if err := json.Unmarshal([]byte(body), &wp); err != nil {
		t.Fatal(err)
	}
	code, body := e.apiCall(t, aliceKey, "POST", "/api/v1/runs/"+wp.RunID+"/summaries",
		`{"summaries":[{"id":"itm_alice","stage":"light","summary_md":"Alice summary text."}]}`)
	if code != http.StatusOK || !strings.Contains(body, `"accepted":1`) {
		t.Fatalf("submit = %d %s", code, body)
	}

	file, err := os.ReadFile(filepath.Join(e.briefDir, "alice", wp.Day+".md"))
	if err != nil || !strings.Contains(string(file), "Alice summary text.") {
		t.Fatalf("file = %v %q", err, file)
	}
	if _, err := os.Stat(filepath.Join(e.briefDir, "bob")); err == nil {
		t.Error("alice's run wrote into bob's folder")
	}

	c := e.client()
	if resp := e.login(t, c, "alice", "correct horse battery"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	resp, err := c.Get(e.srv.URL + "/read")
	if err != nil {
		t.Fatal(err)
	}
	page := e.body(t, resp)
	if !strings.Contains(page, "Alice summary text.") || !strings.Contains(page, "<article") {
		t.Fatalf("alice's /read lacks her summary:\n%s", page)
	}
	main := page[strings.Index(page, "<main"):strings.Index(page, "</main>")]
	for _, banned := range []string{"<button", "<form", "data-action"} {
		if strings.Contains(main, banned) {
			t.Errorf("/read contains %q in the reading flow", banned)
		}
	}
	if strings.Contains(page, "bob item") {
		t.Error("another user's item on /read")
	}

	// Bob sees an empty page, not Alice's summary.
	bc, _ := e.signedIn(t, "carol")
	resp, err = bc.Get(e.srv.URL + "/read")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.body(t, resp); strings.Contains(got, "Alice summary text.") {
		t.Error("another user's summary on /read")
	}
}

func TestNextRunClosesPreviousDayFile(t *testing.T) {
	e := setup(t, nil, false)
	uid, key := e.apiUser(t, "alice")
	// A summarized run from an earlier day whose item was archived since.
	e.exec(t, `INSERT INTO runs (id, user_id, started_at, summaries_at, day) VALUES ('r_old', ?, 'n', 'n', '2000-01-01')`, uid)
	e.exec(t, `INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, light_summary, batch_date, created_at, updated_at)
		VALUES ('itm_old', ?, 'src_alice', '2', 'article', 'https://example.com/o', 'Old one', 'archived', 's', '2000-01-01', 'c', 'c')`, uid)

	if code, body := e.apiCall(t, key, "POST", "/api/v1/runs", ""); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	file, err := os.ReadFile(filepath.Join(e.briefDir, "alice", "2000-01-01.md"))
	if err != nil || !strings.Contains(string(file), "final: true") || !strings.Contains(string(file), "**archived**") {
		t.Fatalf("earlier day not closed: %v\n%s", err, file)
	}
}

func (e *env) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, err := e.st.DB().Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}
