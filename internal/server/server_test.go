package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Jolls/Siftstr/internal/auth"
	"github.com/Jolls/Siftstr/internal/store"
)

type env struct {
	srv  *httptest.Server
	auth *auth.Service
}

func setup(t *testing.T, ready func(context.Context) error, secure bool) *env {
	t.Helper()
	st, err := store.OpenFile(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a, err := auth.New(st.DB(), []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if ready == nil {
		ready = func(context.Context) error { return nil }
	}
	h, err := New(Deps{Ready: ready, Auth: a, SecureCookies: secure})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &env{srv: srv, auth: a}
}

// client returns an HTTP client with a cookie jar that does not follow redirects.
func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (e *env) body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// login performs the full form flow and returns the response to the POST.
func (e *env) login(t *testing.T, c *http.Client, user, pass string) *http.Response {
	t.Helper()
	resp, err := c.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	m := csrfRe.FindStringSubmatch(e.body(t, resp))
	if m == nil {
		t.Fatal("no csrf field on login form")
	}
	resp, err = c.PostForm(e.srv.URL+"/login", url.Values{"csrf": {m[1]}, "username": {user}, "password": {pass}})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealthz(t *testing.T) {
	e := setup(t, nil, false)
	resp, _ := http.Get(e.srv.URL + "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	e = setup(t, func(context.Context) error { return errors.New("db down") }, false)
	resp, _ = http.Get(e.srv.URL + "/healthz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestProtectedRoutesRedirectToLogin(t *testing.T) {
	e := setup(t, nil, false)
	c := e.client()
	for _, p := range []string{"/", "/today", "/backlog"} {
		resp, _ := c.Get(e.srv.URL + p)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("%s: %d -> %q", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestLoginFlow(t *testing.T) {
	e := setup(t, nil, true)
	ctx := context.Background()
	_, _ = e.auth.CreateUser(ctx, "alice", "password-one", auth.RoleUser)
	c := e.client()

	// Wrong password.
	resp := e.login(t, c, "alice", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login = %d", resp.StatusCode)
	}

	// Right password sets a hardened session cookie.
	resp = e.login(t, c, "alice", "password-one")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/today" {
		t.Fatalf("login = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var sc *http.Cookie
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			sc = ck
		}
	}
	if sc == nil || !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie = %+v", sc)
	}

	// The session reaches protected pages. The test server is plain HTTP and
	// the cookie is Secure, so send it by hand.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/today", nil)
	req.AddCookie(sc)
	resp, _ = http.DefaultClient.Do(req)
	page := e.body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Nothing to sift yet.") {
		t.Fatalf("/today = %d", resp.StatusCode)
	}
	if !strings.Contains(page, `action="/logout"`) {
		t.Error("no logout form")
	}
}

func TestLoginRejectsMissingOrWrongCSRF(t *testing.T) {
	e := setup(t, nil, false)
	_, _ = e.auth.CreateUser(context.Background(), "alice", "password-one", auth.RoleUser)
	c := e.client()
	_, _ = c.Get(e.srv.URL + "/login") // sets the login cookie
	resp, _ := c.PostForm(e.srv.URL+"/login", url.Values{"csrf": {"forged"}, "username": {"alice"}, "password": {"password-one"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged csrf = %d", resp.StatusCode)
	}
	resp, _ = http.PostForm(e.srv.URL+"/login", url.Values{"username": {"alice"}, "password": {"password-one"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no csrf = %d", resp.StatusCode)
	}
}

func TestLogoutNeedsCSRFAndEndsSession(t *testing.T) {
	e := setup(t, nil, false)
	_, _ = e.auth.CreateUser(context.Background(), "alice", "password-one", auth.RoleUser)
	c := e.client()
	if resp := e.login(t, c, "alice", "password-one"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d", resp.StatusCode)
	}

	resp, _ := c.Get(e.srv.URL + "/today")
	m := csrfRe.FindStringSubmatch(e.body(t, resp))
	if m == nil {
		t.Fatal("no csrf on authenticated page")
	}

	resp, _ = c.PostForm(e.srv.URL+"/logout", url.Values{})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without csrf = %d", resp.StatusCode)
	}
	resp, _ = c.PostForm(e.srv.URL+"/logout", url.Values{"csrf": {m[1]}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout = %d", resp.StatusCode)
	}
	resp, _ = c.Get(e.srv.URL + "/today")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("after logout /today = %d", resp.StatusCode)
	}
}

// A CSRF token from one user's session must not work in another's.
func TestCSRFIsPerSession(t *testing.T) {
	e := setup(t, nil, false)
	ctx := context.Background()
	_, _ = e.auth.CreateUser(ctx, "alice", "password-one", auth.RoleUser)
	_, _ = e.auth.CreateUser(ctx, "bob", "password-two", auth.RoleUser)
	ca, cb := e.client(), e.client()
	e.login(t, ca, "alice", "password-one")
	e.login(t, cb, "bob", "password-two")

	resp, _ := ca.Get(e.srv.URL + "/today")
	aliceToken := csrfRe.FindStringSubmatch(e.body(t, resp))[1]
	resp, _ = cb.PostForm(e.srv.URL+"/logout", url.Values{"csrf": {aliceToken}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-session csrf = %d", resp.StatusCode)
	}
}

func TestSecurityHeadersAndStatic(t *testing.T) {
	e := setup(t, nil, false)
	resp, _ := http.Get(e.srv.URL + "/login")
	if resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("login page missing CSP or no-store")
	}
	if resp, _ := http.Get(e.srv.URL + "/static/app.css"); resp.StatusCode != http.StatusOK {
		t.Errorf("static = %d", resp.StatusCode)
	}
	if resp, _ := http.Get(e.srv.URL + "/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown = %d", resp.StatusCode)
	}
}
