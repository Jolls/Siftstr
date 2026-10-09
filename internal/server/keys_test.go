package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var newKeyRe = regexp.MustCompile(`<pre><code>(sft_[A-Za-z0-9_-]+)</code></pre>`)

func TestAPIKeyLifecycleFromSettings(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")

	// Creating redirects, so refreshing cannot submit the form again.
	resp := e.post(t, c, "/settings/keys", url.Values{"csrf": {csrf}, "label": {"claude-task"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/keys" {
		t.Fatalf("create = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err := c.Get(e.srv.URL + "/settings/keys")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("the page that shows a new key must not be cached")
	}
	m := newKeyRe.FindStringSubmatch(e.body(t, resp))
	if m == nil {
		t.Fatal("new key not shown after redirect")
	}
	key := m[1]
	var n int
	_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n)
	if n != 1 {
		t.Fatalf("keys = %d", n)
	}

	// The key works on the API right away.
	if code, _ := e.apiCall(t, key, "GET", "/api/v1/ping", ""); code != http.StatusOK {
		t.Fatalf("new key rejected: %d", code)
	}

	// Reloading never shows it again; the list has the label and an active row.
	_, body := e.get(t, c, "/settings/keys")
	if strings.Contains(body, key) {
		t.Fatal("key shown again after creation")
	}
	if !strings.Contains(body, "claude-task") || !strings.Contains(body, "active") {
		t.Fatalf("list missing the key row:\n%s", body)
	}

	var id string
	if err := e.st.DB().QueryRow(`SELECT id FROM api_keys`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if resp := e.post(t, c, "/settings/keys/"+id+"/revoke", url.Values{"csrf": {csrf}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}
	if code, _ := e.apiCall(t, key, "GET", "/api/v1/ping", ""); code != http.StatusUnauthorized {
		t.Fatalf("revoked key still works: %d", code)
	}
	_, body = e.get(t, c, "/settings/keys")
	if !strings.Contains(body, "revoked 20") || strings.Contains(body, "/revoke") {
		t.Fatalf("revoked row wrong:\n%s", body)
	}
}

func TestAPIKeyPageIsScopedAndProtected(t *testing.T) {
	e := setup(t, nil, false)
	_, _ = e.apiUser(t, "bob") // bob owns a key labelled "test"
	c, csrf := e.signedIn(t, "alice")

	_, body := e.get(t, c, "/settings/keys")
	if strings.Contains(body, "test") && strings.Contains(body, "active") {
		t.Fatal("alice sees bob's key")
	}

	bob, _ := e.auth.UserByUsername(context.Background(), "bob")
	keys, _ := e.auth.ListAPIKeys(context.Background(), bob.ID)
	if resp := e.post(t, c, "/settings/keys/"+keys[0].ID+"/revoke", url.Values{"csrf": {csrf}}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoking another user's key = %d, want 404", resp.StatusCode)
	}
	if keys, _ = e.auth.ListAPIKeys(context.Background(), bob.ID); keys[0].RevokedAt != nil {
		t.Fatal("bob's key was revoked")
	}

	// CSRF and login are required.
	if resp := e.post(t, c, "/settings/keys", url.Values{"label": {"x"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("create without csrf = %d", resp.StatusCode)
	}
	anon := e.client()
	if resp, _ := anon.Get(e.srv.URL + "/settings/keys"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous = %d", resp.StatusCode)
	}

	long := url.Values{"csrf": {csrf}, "label": {strings.Repeat("a", 81)}}
	if resp := e.post(t, c, "/settings/keys", long); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("long label = %d", resp.StatusCode)
	}
}

func TestKeyLabelLimitCountsCharactersNotBytes(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	label := strings.Repeat("鍵", 60) // 60 characters, 180 bytes
	if resp := e.post(t, c, "/settings/keys", url.Values{"csrf": {csrf}, "label": {label}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("60-character label = %d", resp.StatusCode)
	}
}

func TestOneTimeSecretsExpireAndAreTakenOnce(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	o := newOneTimeSecrets(time.Minute)
	o.now = func() time.Time { return now }
	o.put("s1", "secret")
	if v, ok := o.take("s1"); !ok || v != "secret" {
		t.Fatalf("take = %q, %v", v, ok)
	}
	if _, ok := o.take("s1"); ok {
		t.Fatal("secret returned twice")
	}
	o.put("s2", "late")
	now = now.Add(2 * time.Minute)
	if _, ok := o.take("s2"); ok {
		t.Fatal("expired secret returned")
	}
	if _, ok := o.take("other-session"); ok {
		t.Fatal("secret leaked to another session")
	}
}
