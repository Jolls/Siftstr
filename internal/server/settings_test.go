package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// signedIn creates a user, logs in and returns the client and its CSRF token.
func (e *env) signedIn(t *testing.T, name string) (*http.Client, string) {
	t.Helper()
	if _, err := e.auth.CreateUser(context.Background(), name, "correct horse battery", "user"); err != nil {
		t.Fatal(err)
	}
	c := e.client()
	if resp := e.login(t, c, name, "correct horse battery"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	resp, err := c.Get(e.srv.URL + "/settings/destinations")
	if err != nil {
		t.Fatal(err)
	}
	m := csrfRe.FindStringSubmatch(e.body(t, resp))
	if m == nil {
		t.Fatal("no csrf token on settings page")
	}
	return c, m[1]
}

func (e *env) post(t *testing.T, c *http.Client, path string, v url.Values) *http.Response {
	t.Helper()
	resp, err := c.PostForm(e.srv.URL+path, v)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func (e *env) get(t *testing.T, c *http.Client, path string) (int, string) {
	t.Helper()
	resp, err := c.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, e.body(t, resp)
}

func TestSettingsRequireLoginAndCSRF(t *testing.T) {
	e := setup(t, nil, false)
	c := e.client()
	for _, p := range []string{"/settings", "/settings/sources", "/settings/destinations"} {
		resp, _ := c.Get(e.srv.URL + p)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("%s: %d -> %q", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	c, _ = e.signedIn(t, "alice")
	resp := e.post(t, c, "/settings/destinations/miniflux", url.Values{"base_url": {"https://mf.example.com"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("post without csrf = %d", resp.StatusCode)
	}
}

func TestDestinationLifecycleNeverEchoesSecret(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	form := func(base, secret string) url.Values {
		return url.Values{"csrf": {csrf}, "base_url": {base}, "secret": {secret}}
	}

	resp := e.post(t, c, "/settings/destinations/miniflux", form("https://mf.example.com", "tok-SECRET"))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	code, body := e.get(t, c, "/settings/destinations?notice=saved")
	if code != 200 || strings.Contains(body, "tok-SECRET") || !strings.Contains(body, "https://mf.example.com") ||
		!strings.Contains(body, "Saved. Enter a new one") || !strings.Contains(body, "Saved.") {
		t.Fatalf("page after create (%d) wrong:\n%s", code, body)
	}

	// A blank secret keeps the saved one.
	e.post(t, c, "/settings/destinations/miniflux", form("https://mf2.example.com", ""))
	var base, has = "", false
	if err := e.st.DB().QueryRow(`SELECT base_url, secret_enc IS NOT NULL FROM connections`).Scan(&base, &has); err != nil {
		t.Fatal(err)
	}
	if base != "https://mf2.example.com" || !has {
		t.Fatalf("after blank-secret save: %q %v", base, has)
	}

	// An explicit clear removes it.
	v := form("https://mf2.example.com", "")
	v.Set("clear_secret", "1")
	e.post(t, c, "/settings/destinations/miniflux", v)
	_ = e.st.DB().QueryRow(`SELECT secret_enc IS NOT NULL FROM connections`).Scan(&has)
	if has {
		t.Fatal("secret not cleared")
	}

	resp = e.post(t, c, "/settings/destinations/miniflux/delete", url2(csrf))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	var n int
	_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM connections`).Scan(&n)
	if n != 0 {
		t.Fatal("connection not deleted")
	}
}

func url2(csrf string) url.Values { return url.Values{"csrf": {csrf}} }

func TestDestinationValidationAndUnknownKind(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	resp := e.post(t, c, "/settings/destinations/miniflux", url.Values{"csrf": {csrf}, "base_url": {"ftp://x.example"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad url = %d", resp.StatusCode)
	}
	resp = e.post(t, c, "/settings/destinations/bogus", url.Values{"csrf": {csrf}, "base_url": {"https://x.example"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown kind = %d", resp.StatusCode)
	}
}

func TestSourcesSaveAndIsolation(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	var aliceID, bobID string
	_ = e.st.DB().QueryRow(`SELECT id FROM users WHERE username = 'alice'`).Scan(&aliceID)
	if _, err := e.auth.CreateUser(context.Background(), "bob", "correct horse battery", "user"); err != nil {
		t.Fatal(err)
	}
	_ = e.st.DB().QueryRow(`SELECT id FROM users WHERE username = 'bob'`).Scan(&bobID)
	for _, q := range []struct{ id, user, name string }{{"s_a", aliceID, "Alice Feed"}, {"s_b", bobID, "Bob Feed"}} {
		if _, err := e.st.DB().Exec(`INSERT INTO sources (id, user_id, kind, external_id, name) VALUES (?, ?, 'miniflux_feed', '1', ?)`, q.id, q.user, q.name); err != nil {
			t.Fatal(err)
		}
	}

	code, body := e.get(t, c, "/settings/sources")
	if code != 200 || !strings.Contains(body, "Alice Feed") || strings.Contains(body, "Bob Feed") {
		t.Fatalf("list (%d) wrong", code)
	}

	save := url.Values{"csrf": {csrf}, "granularity": {"digest"}, "max_depth": {"light"}, "carryover": {"drop"}, "prompt_override": {"short"}}
	if resp := e.post(t, c, "/settings/sources/s_a", save); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	var gran string
	var enabled bool
	_ = e.st.DB().QueryRow(`SELECT granularity, enabled FROM sources WHERE id = 's_a'`).Scan(&gran, &enabled)
	if gran != "digest" || enabled { // checkbox omitted = disabled
		t.Fatalf("saved %q enabled=%v", gran, enabled)
	}

	if resp := e.post(t, c, "/settings/sources/s_b", save); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other user's source = %d, want 404", resp.StatusCode)
	}
	_ = e.st.DB().QueryRow(`SELECT granularity FROM sources WHERE id = 's_b'`).Scan(&gran)
	if gran != "individual" {
		t.Fatal("bob's source was modified")
	}

	save.Set("granularity", "weekly")
	if resp := e.post(t, c, "/settings/sources/s_a", save); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid = %d", resp.StatusCode)
	}
}

func TestIngestSettings(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	_, body := e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `name="max_age_days" min="0" max="3650" value="14"`) {
		t.Fatal("default max age not shown")
	}
	v := url.Values{"csrf": {csrf}, "max_age_days": {"30"}, "excerpt_length": {"300"}}
	if resp := e.post(t, c, "/settings/ingest", v); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/sources")
	if !strings.Contains(body, `value="30"`) || !strings.Contains(body, `value="300"`) {
		t.Fatal("saved values not shown")
	}
	for _, bad := range []url.Values{
		{"csrf": {csrf}, "max_age_days": {"-1"}, "excerpt_length": {"300"}},
		{"csrf": {csrf}, "max_age_days": {"x"}, "excerpt_length": {"300"}},
		{"csrf": {csrf}, "max_age_days": {"5"}, "excerpt_length": {"10"}},
	} {
		if resp := e.post(t, c, "/settings/ingest", bad); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%v = %d", bad, resp.StatusCode)
		}
	}
}

func TestKeepDestinationsSaveAndIsolation(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	c2, _ := e.signedIn(t, "bob")
	_, body := e.get(t, c, "/settings/destinations")
	// Defaults: video goes to every capable destination, podcasts to Karakeep.
	if strings.Count(body, " checked>") != 3 {
		t.Fatalf("defaults: want 3 checked boxes\n%s", body)
	}
	v := url.Values{"csrf": {csrf}, "video": {"metube"}}
	if resp := e.post(t, c, "/settings/destinations/keep", v); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body = e.get(t, c, "/settings/destinations")
	if strings.Count(body, " checked>") != 1 || !strings.Contains(body, `name="video" value="metube" checked>`) {
		t.Fatalf("saved choice not shown\n%s", body)
	}
	_, body = e.get(t, c2, "/settings/destinations")
	if strings.Count(body, " checked>") != 3 {
		t.Fatal("alice's choice leaked to bob")
	}
	// Without CSRF the save is refused.
	if resp := e.post(t, c, "/settings/destinations/keep", url.Values{"video": {"karakeep"}}); resp.StatusCode == http.StatusSeeOther {
		t.Fatal("saved without csrf")
	}
}
