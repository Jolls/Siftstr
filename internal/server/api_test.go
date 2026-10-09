package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// apiCall sends a Bearer-authenticated JSON request.
func (e *env) apiCall(t *testing.T, key, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// apiUser creates a user with one source and one pending item, and an API key.
func (e *env) apiUser(t *testing.T, name string) (userID, key string) {
	t.Helper()
	u, err := e.auth.CreateUser(context.Background(), name, "correct horse battery", "user")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err = e.auth.CreateAPIKey(context.Background(), u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	src := "src_" + name
	if _, err := e.st.DB().Exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES (?, ?, 'miniflux_feed', '1', ?)`, src, u.ID, name+" feed"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.DB().Exec(`INSERT INTO items (id, user_id, source_id, external_id, media_type, url, title, state, created_at, updated_at)
		VALUES (?, ?, ?, '1', 'article', 'https://example.com/x', ?, 'pending_light', 'c', 'c')`, "itm_"+name, u.ID, src, name+" item"); err != nil {
		t.Fatal(err)
	}
	return u.ID, key
}

func TestRunsAPIRequiresAPIKey(t *testing.T) {
	e := setup(t, nil, false)
	if code, _ := e.apiCall(t, "", "POST", "/api/v1/runs", ""); code != http.StatusUnauthorized {
		t.Fatalf("no key = %d", code)
	}
	if code, _ := e.apiCall(t, "bogus", "POST", "/api/v1/runs/run_x/summaries", `{}`); code != http.StatusUnauthorized {
		t.Fatalf("bad key = %d", code)
	}
	// A session cookie is not accepted on /api.
	c, _ := e.signedIn(t, "alice")
	resp, err := c.Post(e.srv.URL+"/api/v1/runs", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session on /api = %d", resp.StatusCode)
	}
}

func TestRunRoundTripOverHTTP(t *testing.T) {
	e := setup(t, nil, false)
	_, key := e.apiUser(t, "alice")

	code, body := e.apiCall(t, key, "POST", "/api/v1/runs", "")
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var wp struct {
		RunID string `json:"run_id"`
		Items []struct {
			ID    string `json:"id"`
			Stage string `json:"stage"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &wp); err != nil {
		t.Fatal(err)
	}
	if len(wp.Items) != 1 || wp.Items[0].ID != "itm_alice" || wp.Items[0].Stage != "light" {
		t.Fatalf("package = %s", body)
	}

	payload := `{"summaries":[{"id":"itm_alice","stage":"light","summary_md":"A short summary."}]}`
	code, body = e.apiCall(t, key, "POST", "/api/v1/runs/"+wp.RunID+"/summaries", payload)
	if code != http.StatusOK || !strings.Contains(body, `"accepted":1`) {
		t.Fatalf("submit = %d %s", code, body)
	}
	var state string
	_ = e.st.DB().QueryRow(`SELECT state FROM items WHERE id = 'itm_alice'`).Scan(&state)
	if state != "light" {
		t.Fatalf("state = %s", state)
	}
	// Sending the batch again is harmless.
	code, body = e.apiCall(t, key, "POST", "/api/v1/runs/"+wp.RunID+"/summaries", payload)
	if code != http.StatusOK || !strings.Contains(body, `"accepted":0`) {
		t.Fatalf("replay = %d %s", code, body)
	}
}

func TestRunsAPIIsolationBetweenUsers(t *testing.T) {
	e := setup(t, nil, false)
	_, aliceKey := e.apiUser(t, "alice")
	_, bobKey := e.apiUser(t, "bob")

	_, body := e.apiCall(t, aliceKey, "POST", "/api/v1/runs", "")
	if strings.Contains(body, "bob") {
		t.Fatalf("alice's package mentions bob: %s", body)
	}
	var wp struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(body), &wp)

	// Bob cannot submit into Alice's run, and cannot summarize her item.
	code, _ := e.apiCall(t, bobKey, "POST", "/api/v1/runs/"+wp.RunID+"/summaries", `{"summaries":[]}`)
	if code != http.StatusNotFound {
		t.Fatalf("other user's run = %d, want 404", code)
	}
	_, bobBody := e.apiCall(t, bobKey, "POST", "/api/v1/runs", "")
	var bw struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(bobBody), &bw)
	code, body = e.apiCall(t, bobKey, "POST", "/api/v1/runs/"+bw.RunID+"/summaries",
		`{"summaries":[{"id":"itm_alice","stage":"light","summary_md":"hijack"}]}`)
	if code != http.StatusOK || !strings.Contains(body, `"accepted":0`) {
		t.Fatalf("cross-user submit = %d %s", code, body)
	}
	var state string
	_ = e.st.DB().QueryRow(`SELECT state FROM items WHERE id = 'itm_alice'`).Scan(&state)
	if state != "pending_light" {
		t.Fatalf("alice's item changed to %s", state)
	}
}

func TestSubmitRejectsBadJSON(t *testing.T) {
	e := setup(t, nil, false)
	_, key := e.apiUser(t, "alice")
	_, body := e.apiCall(t, key, "POST", "/api/v1/runs", "")
	var wp struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(body), &wp)
	if code, _ := e.apiCall(t, key, "POST", "/api/v1/runs/"+wp.RunID+"/summaries", `{not json`); code != http.StatusBadRequest {
		t.Fatalf("bad json = %d", code)
	}
}

func TestPromptsAndTimezoneSettings(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")

	_, body := e.get(t, c, "/settings/prompts")
	if !strings.Contains(body, "Light summary") || !strings.Contains(body, "Digest") {
		t.Fatal("prompts page missing fields")
	}
	save := url.Values{"csrf": {csrf}, "name": {"light"}, "value": {"my light prompt"}}
	if resp := e.post(t, c, "/settings/prompts", save); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/prompts")
	if !strings.Contains(body, ">my light prompt</textarea>") {
		t.Fatal("saved prompt not shown")
	}
	save.Set("name", "bogus")
	if resp := e.post(t, c, "/settings/prompts", save); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown prompt = %d", resp.StatusCode)
	}

	tz := url.Values{"csrf": {csrf}, "timezone": {"Europe/Berlin"}}
	if resp := e.post(t, c, "/settings/timezone", tz); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("timezone = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `<option value="Europe/Berlin" selected>`) {
		t.Fatal("timezone not shown")
	}
	tz.Set("timezone", "Nowhere/Land")
	if resp := e.post(t, c, "/settings/timezone", tz); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad timezone = %d", resp.StatusCode)
	}
}

func TestSubmitOversizedBodyIs413(t *testing.T) {
	e := setup(t, nil, false)
	_, key := e.apiUser(t, "alice")
	_, body := e.apiCall(t, key, "POST", "/api/v1/runs", "")
	var wp struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal([]byte(body), &wp)
	huge := `{"summaries":[{"id":"x","stage":"light","summary_md":"` + strings.Repeat("a", maxSummariesBody) + `"}]}`
	if code, _ := e.apiCall(t, key, "POST", "/api/v1/runs/"+wp.RunID+"/summaries", huge); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized = %d, want 413", code)
	}
}
