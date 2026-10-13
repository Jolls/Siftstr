package server

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Jolls/Siftstr/internal/auth"
)

// postRaw sends body to path with the given content type and optional extra
// headers, and closes nothing: the caller reads the response.
func (e *env) postRaw(t *testing.T, c *http.Client, path, contentType, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// loginToken fetches the login form (which sets the login cookie) and
// returns its CSRF token.
func (e *env) loginToken(t *testing.T, c *http.Client) string {
	t.Helper()
	resp, err := c.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	m := csrfRe.FindStringSubmatch(e.body(t, resp))
	if m == nil {
		t.Fatal("no csrf field on login form")
	}
	return m[1]
}

func multipartBody(t *testing.T, fields map[string]string, fileBytes int) (body, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	// The CSRF field goes first so a handler that stops early still sees it.
	if v, ok := fields["csrf"]; ok {
		_ = w.WriteField("csrf", v)
	}
	for k, v := range fields {
		if k != "csrf" {
			_ = w.WriteField(k, v)
		}
	}
	if fileBytes > 0 {
		fw, err := w.CreateFormFile("upload", "big.bin")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write(bytes.Repeat([]byte("x"), fileBytes))
	}
	_ = w.Close()
	return buf.String(), w.FormDataContentType()
}

// padTo returns prefix extended with filler to exactly n bytes.
func padTo(prefix string, n int) string {
	return prefix + strings.Repeat("a", n-len(prefix))
}

func newLimitEnv(t *testing.T) *env {
	t.Helper()
	e := setup(t, nil, false)
	_, _ = e.auth.CreateUser(context.Background(), "alice", "password-one", auth.RoleUser)
	return e
}

func TestCharLoginWithPaddingUnderLimit(t *testing.T) {
	e := newLimitEnv(t)
	c := e.client()
	form := url.Values{"csrf": {e.loginToken(t, c)}, "username": {"alice"}, "password": {"password-one"}, "pad": {strings.Repeat("a", 8000)}}
	resp := e.postRaw(t, c, "/login", "application/x-www-form-urlencoded", form.Encode(), nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/today" {
		t.Fatalf("login = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestCharSettingsPostWithHeaderCSRF(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	resp := e.postRaw(t, c, "/settings/theme", "application/x-www-form-urlencoded", "theme=dark", map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("header csrf save = %d", resp.StatusCode)
	}
}

func TestCharMultipartLoginAndSettings(t *testing.T) {
	e := newLimitEnv(t)
	c := e.client()
	body, ct := multipartBody(t, map[string]string{"csrf": e.loginToken(t, c), "username": "alice", "password": "password-one"}, 0)
	resp := e.postRaw(t, c, "/login", ct, body, nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/today" {
		t.Fatalf("multipart login = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	c2, csrf := e.signedIn(t, "bob")
	body, ct = multipartBody(t, map[string]string{"csrf": csrf, "theme": "dark"}, 0)
	resp = e.postRaw(t, c2, "/settings/theme", ct, body, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("multipart settings = %d", resp.StatusCode)
	}
}

func TestLoginOversizedBodyIs413AndNotEvaluated(t *testing.T) {
	e := newLimitEnv(t)
	c := e.client()
	// Valid credentials, but the body is one byte over the 16 KB limit.
	form := url.Values{"csrf": {e.loginToken(t, c)}, "username": {"alice"}, "password": {"password-one"}}
	body := padTo(form.Encode()+"&pad=", maxLoginBody+1)
	resp := e.postRaw(t, c, "/login", "application/x-www-form-urlencoded", body, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized login = %d, want 413", resp.StatusCode)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "siftstr_session" && ck.Value != "" {
			t.Fatal("oversized login started a session")
		}
	}
}

func TestSettingsOversizedBodyIs413(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	body := padTo("csrf="+url.QueryEscape(csrf)+"&theme=dark&pad=", maxFormBody+1)
	resp := e.postRaw(t, c, "/settings/theme", "application/x-www-form-urlencoded", body, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized settings post = %d, want 413", resp.StatusCode)
	}
	if _, page := e.get(t, c, "/settings/appearance"); strings.Contains(page, `name="theme" value="dark" checked`) {
		t.Error("the oversized post changed the theme")
	}
}

func TestSettingsOversizedMultipartIs413(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	body, ct := multipartBody(t, map[string]string{"csrf": csrf, "theme": "dark"}, maxFormBody+1)
	resp := e.postRaw(t, c, "/settings/theme", ct, body, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized multipart = %d, want 413", resp.StatusCode)
	}
	if _, page := e.get(t, c, "/settings/appearance"); strings.Contains(page, `name="theme" value="dark" checked`) {
		t.Error("the oversized post changed the theme")
	}
}

func TestLogoutOversizedBodyIs413(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	body := padTo("csrf="+url.QueryEscape(csrf)+"&pad=", maxFormBody+1)
	resp := e.postRaw(t, c, "/logout", "application/x-www-form-urlencoded", body, nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized logout = %d, want 413", resp.StatusCode)
	}
	if status, _ := e.get(t, c, "/today"); status != http.StatusOK {
		t.Errorf("session after the refused logout: /today = %d", status)
	}
}
