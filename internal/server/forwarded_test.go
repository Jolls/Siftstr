package server

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"testing"

	"github.com/Jolls/Siftstr/internal/auth"
)

// loginFrom is like login, but the POST carries an X-Forwarded-For header
// when xff is not empty.
func (e *env) loginFrom(t *testing.T, c *http.Client, user, pass, xff string) *http.Response {
	t.Helper()
	form := url.Values{"csrf": {e.loginToken(t, c)}, "username": {user}, "password": {pass}}
	var hdr map[string]string
	if xff != "" {
		hdr = map[string]string{"X-Forwarded-For": xff}
	}
	return e.postRaw(t, c, "/login", "application/x-www-form-urlencoded", form.Encode(), hdr)
}

func TestForwardedForIgnoredWithoutTrustedProxies(t *testing.T) {
	e := setup(t, nil, false)
	_, _ = e.auth.CreateUser(context.Background(), "alice", "password-one", auth.RoleUser)
	c := e.client()
	for i := 0; i < 20; i++ {
		resp := e.loginFrom(t, c, fmt.Sprintf("nobody%d", i), "bad", fmt.Sprintf("198.51.100.%d", i))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad login %d = %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := e.loginFrom(t, c, "alice", "password-one", "198.51.100.200")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("login after the IP limit = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLoginBehindTrustedProxyCountsEachClient(t *testing.T) {
	e := setupDeps(t, nil, false, func(d *Deps) {
		d.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}
	})
	_, _ = e.auth.CreateUser(context.Background(), "alice", "password-one", auth.RoleUser)
	c := e.client()
	for i := 0; i < 20; i++ {
		resp := e.loginFrom(t, c, fmt.Sprintf("nobody%d", i), "bad", "198.51.100.1")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad login %d = %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := e.loginFrom(t, c, "alice", "password-one", "198.51.100.1")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("blocked client = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = e.loginFrom(t, e.client(), "alice", "password-one", "198.51.100.2")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/today" {
		t.Fatalf("other client = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp.Body.Close()
}
