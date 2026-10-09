package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// seedLight gives a signed-in user a source and a light item.
func (e *env) seedLight(t *testing.T, name, itemID string) {
	t.Helper()
	var uid string
	if err := e.st.DB().QueryRow(`SELECT id FROM users WHERE username = ?`, name).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	src := "src_" + name
	if _, err := e.st.DB().Exec(`INSERT OR IGNORE INTO sources (id, user_id, kind, external_id, name) VALUES (?, ?, 'miniflux_feed', '1', ?)`, src, uid, name); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB().Exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'article', 'https://example.com/x', 't', 'light', 'c', 'c')`, itemID, uid, src, itemID); err != nil {
		t.Fatal(err)
	}
}

func (e *env) sync(t *testing.T, c *http.Client, csrf, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/sync/actions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, e.body(t, resp)
}

func TestSyncActionsContract(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	e.seedLight(t, "alice", "itm_1")

	batch := `{"actions":[{"action_id":"a1","subject_type":"item","subject_id":"itm_1","kind":"archive","at":"2026-10-09T07:12:03Z"},
		{"action_id":"a2","subject_id":"itm_nope","kind":"keep","at":"2026-10-09T07:12:04Z"}]}`
	code, body := e.sync(t, c, csrf, batch)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	var got struct {
		Results []map[string]string `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || len(got.Results) != 2 {
		t.Fatalf("body %s: %v", body, err)
	}
	want := []map[string]string{
		{"action_id": "a1", "status": "applied", "subject_type": "item", "subject_id": "itm_1", "state": "archived"},
		{"action_id": "a2", "status": "rejected", "reason": "unknown subject", "subject_type": "item", "subject_id": "itm_nope", "state": ""},
	}
	for i, w := range want {
		for k, v := range w {
			if got.Results[i][k] != v {
				t.Errorf("result %d %s = %q, want %q (%s)", i, k, got.Results[i][k], v, body)
			}
		}
	}

	// The same batch again changes nothing and says so.
	_, body = e.sync(t, c, csrf, batch)
	if !strings.Contains(body, `"status":"duplicate"`) || !strings.Contains(body, `"reason":"unknown subject"`) {
		t.Fatalf("replay: %s", body)
	}
}

func TestSyncActionsRequiresSessionAndCSRF(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	e.seedLight(t, "alice", "itm_1")
	batch := `{"actions":[{"action_id":"a1","subject_id":"itm_1","kind":"archive","at":"2026-10-09T07:12:03Z"}]}`

	if code, _ := e.sync(t, c, "", batch); code != http.StatusForbidden {
		t.Fatalf("no csrf = %d", code)
	}
	if code, _ := e.sync(t, c, "wrong", batch); code != http.StatusForbidden {
		t.Fatalf("bad csrf = %d", code)
	}
	if code, _ := e.sync(t, e.client(), csrf, batch); code != http.StatusSeeOther {
		t.Fatalf("no session = %d", code)
	}
	// An API key is not accepted here.
	req, _ := http.NewRequest("POST", e.srv.URL+"/sync/actions", strings.NewReader(batch))
	_, key := e.apiUser(t, "bob")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := e.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("api key on /sync = %d", resp.StatusCode)
	}
	var state string
	e.st.DB().QueryRow(`SELECT state FROM items WHERE id = 'itm_1'`).Scan(&state)
	if state != "light" {
		t.Fatalf("rejected requests changed state to %s", state)
	}
}

func TestSyncActionsRejectsBadBodies(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	if code, _ := e.sync(t, c, csrf, `not json`); code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", code)
	}
	var sb strings.Builder
	sb.WriteString(`{"actions":[`)
	for i := 0; i <= maxSyncActions; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"action_id":"x","subject_id":"y","kind":"keep"}`)
	}
	sb.WriteString(`]}`)
	if code, _ := e.sync(t, c, csrf, sb.String()); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize batch = %d", code)
	}
}

func TestSyncActionsIsolationBetweenUsers(t *testing.T) {
	e := setup(t, nil, false)
	ca, csrfA := e.signedIn(t, "alice")
	cb, csrfB := e.signedIn(t, "bob")
	e.seedLight(t, "alice", "itm_alice")
	e.seedLight(t, "bob", "itm_bob")

	code, body := e.sync(t, cb, csrfB, `{"actions":[{"action_id":"b1","subject_id":"itm_alice","kind":"archive"}]}`)
	if code != http.StatusOK || !strings.Contains(body, `"unknown subject"`) || strings.Contains(body, `"state":"light"`) {
		t.Fatalf("bob on alice's item: %d %s", code, body)
	}
	e.sync(t, ca, csrfA, `{"actions":[{"action_id":"a1","subject_id":"itm_alice","kind":"keep"}]}`)
	// Bob can neither undo Alice's action nor reuse its id.
	_, body = e.sync(t, cb, csrfB, `{"actions":[{"action_id":"b2","subject_id":"itm_bob","kind":"undo","target_action_id":"a1"},
		{"action_id":"a1","subject_id":"itm_bob","kind":"archive"}]}`)
	if !strings.Contains(body, "nothing to undo") || !strings.Contains(body, "action id in use") {
		t.Fatalf("cross-user undo/reuse: %s", body)
	}
	var s1, s2 string
	e.st.DB().QueryRow(`SELECT state FROM items WHERE id = 'itm_alice'`).Scan(&s1)
	e.st.DB().QueryRow(`SELECT state FROM items WHERE id = 'itm_bob'`).Scan(&s2)
	if s1 != "kept" || s2 != "light" {
		t.Fatalf("alice %s, bob %s", s1, s2)
	}
}

func TestTodayAndBacklogShowItemsAndActions(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	e.seedLight(t, "alice", "itm_new")
	e.seedLight(t, "alice", "itm_old")
	today := time.Now().UTC().Format("2006-01-02")
	e.st.DB().Exec(`UPDATE items SET batch_date = ?, light_summary = 'a <b>summary</b>', title = 'Fresh one' WHERE id = 'itm_new'`, today)
	e.st.DB().Exec(`UPDATE items SET batch_date = '2000-01-01', title = 'Stale one' WHERE id = 'itm_old'`)

	get := func(path string) string {
		resp, err := c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		return e.body(t, resp)
	}
	page := get("/today")
	if !strings.Contains(page, "Fresh one") || strings.Contains(page, "Stale one") {
		t.Fatalf("today: %s", page)
	}
	if strings.Contains(page, "<b>summary</b>") {
		t.Fatal("summary not escaped")
	}
	for _, want := range []string{`data-user-id=`, `/static/gestures.js`, `data-subject-type="item"`, `data-state="light"`} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %s", want)
		}
	}
	if b := get("/backlog"); !strings.Contains(b, "Stale one") || strings.Contains(b, "Fresh one") {
		t.Fatalf("backlog: %s", b)
	}

	// Actioned items stay on the page with a badge and an undo.
	e.sync(t, c, csrf, `{"actions":[{"action_id":"a1","subject_id":"itm_new","kind":"keep"}]}`)
	page = get("/today")
	if !strings.Contains(page, "Fresh one") || !strings.Contains(page, `data-state="kept"`) ||
		!strings.Contains(page, `>kept</span>`) || !strings.Contains(page, `data-undo-action="a1"`) {
		t.Fatalf("after keep: %s", page)
	}
}
