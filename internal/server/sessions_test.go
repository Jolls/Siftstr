package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Jolls/Siftstr/internal/auth"
)

func (e *env) setLastSeen(t *testing.T, user string, at time.Time) {
	t.Helper()
	e.exec(t, `UPDATE sessions SET last_seen_at = ? WHERE user_id = ?`, at.UTC().Format(time.RFC3339), e.userID(t, user))
}

func (e *env) sessionCount(t *testing.T, user string) int {
	t.Helper()
	var n int
	if err := e.st.DB().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, e.userID(t, user)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func sessionCookieCleared(resp *http.Response) bool {
	for _, ck := range resp.Cookies() {
		if ck.Name == "siftstr_session" && ck.MaxAge < 0 {
			return true
		}
	}
	return false
}

func TestIdleSessionRedirectsToLogin(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	e.setLastSeen(t, "alice", time.Now().Add(-auth.SessionIdleTimeout-time.Minute))
	resp, _ := c.Get(e.srv.URL + "/today")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("/today = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !sessionCookieCleared(resp) {
		t.Error("session cookie not cleared")
	}
	if resp = e.post(t, c, "/sync/actions", url.Values{}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("/sync/actions = %d", resp.StatusCode)
	}
}

func TestSignOutEverywhereEndsAllOfTheUsersSessions(t *testing.T) {
	e := setup(t, nil, false)
	ca1, tok := e.signedIn(t, "alice")
	ca2 := e.client()
	e.login(t, ca2, "alice", "correct horse battery")
	cb, _ := e.signedIn(t, "bob")

	resp := e.post(t, ca1, "/settings/sessions/end-all", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("end-all = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !sessionCookieCleared(resp) {
		t.Error("session cookie not cleared")
	}
	for name, c := range map[string]*http.Client{"ca1": ca1, "ca2": ca2} {
		if code, _ := e.get(t, c, "/today"); code != http.StatusSeeOther {
			t.Errorf("%s /today = %d", name, code)
		}
	}
	if code, _ := e.get(t, cb, "/today"); code != http.StatusOK {
		t.Errorf("bob /today = %d", code)
	}
	if n := e.sessionCount(t, "alice"); n != 0 {
		t.Errorf("alice sessions = %d", n)
	}
	if n := e.sessionCount(t, "bob"); n != 1 {
		t.Errorf("bob sessions = %d", n)
	}
}

func TestSignOutEverywhereNeedsCSRFAndLogin(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	if resp := e.post(t, c, "/settings/sessions/end-all", url.Values{}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no csrf = %d", resp.StatusCode)
	}
	if n := e.sessionCount(t, "alice"); n != 1 {
		t.Errorf("alice sessions = %d", n)
	}
	anon := e.client()
	if resp, _ := anon.Get(e.srv.URL + "/settings/sessions"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("anon GET = %d", resp.StatusCode)
	}
	if resp := e.post(t, anon, "/settings/sessions/end-all", url.Values{}); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("anon POST = %d", resp.StatusCode)
	}
}

func TestSignOutEverywhereTwoUserIsolation(t *testing.T) {
	e := setup(t, nil, false)
	ca1, aliceTok := e.signedIn(t, "alice")
	ca2 := e.client()
	e.login(t, ca2, "alice", "correct horse battery")
	cb1, bobTok := e.signedIn(t, "bob")
	cb2 := e.client()
	e.login(t, cb2, "bob", "correct horse battery")

	if resp := e.post(t, cb1, "/settings/sessions/end-all", url.Values{"csrf": {aliceTok}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-user csrf = %d", resp.StatusCode)
	}
	if n := e.sessionCount(t, "alice"); n != 2 {
		t.Fatalf("alice sessions = %d", n)
	}
	e.post(t, cb1, "/settings/sessions/end-all", url.Values{"csrf": {bobTok}})
	for name, c := range map[string]*http.Client{"ca1": ca1, "ca2": ca2} {
		if code, _ := e.get(t, c, "/today"); code != http.StatusOK {
			t.Errorf("%s /today = %d", name, code)
		}
	}
	for name, c := range map[string]*http.Client{"cb1": cb1, "cb2": cb2} {
		if code, _ := e.get(t, c, "/today"); code != http.StatusSeeOther {
			t.Errorf("%s /today = %d", name, code)
		}
	}
}

func TestSessionsPageRenders(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	e.login(t, e.client(), "alice", "correct horse battery")
	e.signedIn(t, "bob")
	e.login(t, e.client(), "bob", "correct horse battery")
	e.login(t, e.client(), "bob", "correct horse battery")

	resp, err := c.Get(e.srv.URL + "/settings/sessions")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status %d, cache %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	body := e.body(t, resp)
	for _, want := range []string{`action="/settings/sessions/end-all"`, "data-logout", `name="csrf"`, `aria-current="page"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if n := strings.Count(body, "data-session>"); n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
	if n := strings.Count(body, "data-current-session"); n != 1 {
		t.Errorf("current markers = %d, want 1", n)
	}
	rows, err := e.st.DB().Query(`SELECT id FROM sessions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		if strings.Contains(body, id) {
			t.Error("session id leaked into page")
		}
	}
}

// secureSessionPost signs in by hand (the test server is plain HTTP and the
// cookie is Secure), reads the CSRF token from /today and posts to path.
func secureSessionPost(t *testing.T, e *env, path string) *http.Response {
	t.Helper()
	u, err := e.auth.CreateUser(context.Background(), "alice", "password-one", auth.RoleUser)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := e.auth.NewSession(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	ck := &http.Cookie{Name: "__Host-siftstr_session", Value: token}
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/today", nil)
	req.AddCookie(ck)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	m := csrfRe.FindStringSubmatch(e.body(t, resp))
	if m == nil {
		t.Fatal("no csrf token on /today")
	}
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+path, strings.NewReader(url.Values{"csrf": {m[1]}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(ck)
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLogoutClearsHostPrefixedCookieWhenSecure(t *testing.T) {
	resp := secureSessionPost(t, setup(t, nil, true), "/logout")
	if ck := cookieByName(resp, "__Host-siftstr_session"); ck == nil || ck.MaxAge >= 0 || !ck.Secure || ck.Path != "/" {
		t.Fatalf("cookie = %+v", ck)
	}
}

func TestSignOutEverywhereClearsHostPrefixedCookieWhenSecure(t *testing.T) {
	resp := secureSessionPost(t, setup(t, nil, true), "/settings/sessions/end-all")
	if ck := cookieByName(resp, "__Host-siftstr_session"); ck == nil || ck.MaxAge >= 0 || !ck.Secure || ck.Path != "/" {
		t.Fatalf("cookie = %+v", ck)
	}
}
