package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Jolls/Siftstr/internal/dest"
)

// fakeCheck records what the server asked it to test.
type fakeCheck struct {
	mu    sync.Mutex
	calls [][3]string
	fn    func(kind, base, secret string) error
}

func (f *fakeCheck) check(_ context.Context, kind, base, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [3]string{kind, base, secret})
	if f.fn != nil {
		return f.fn(kind, base, secret)
	}
	return nil
}

func (f *fakeCheck) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeCheck) last() [3]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func checkEnv(t *testing.T, fc *fakeCheck) *env {
	t.Helper()
	return setupDeps(t, nil, false, func(d *Deps) { d.CheckConnection = fc.check })
}

func saveConn(t *testing.T, e *env, c *http.Client, csrf, kind, base, secret string) *http.Response {
	t.Helper()
	return e.post(t, c, "/settings/destinations/"+kind, url.Values{"csrf": {csrf}, "base_url": {base}, "secret": {secret}})
}

func TestDestinationSaveWithoutCheckerIsUnchanged(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	resp := saveConn(t, e, c, csrf, "miniflux", "https://mf.example.com", "tok-SECRET")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/destinations?notice=saved" {
		t.Fatalf("save = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, body := e.get(t, c, "/settings/destinations")
	if !strings.Contains(body, "https://mf.example.com") || strings.Contains(body, "tok-SECRET") {
		t.Fatal("page wrong")
	}
	if !strings.Contains(body, `data-state=""`) || !strings.Contains(body, "Not checked yet") {
		t.Error("never-checked state not shown")
	}
}

func TestDestinationSaveRunsCheckAndShowsResult(t *testing.T) {
	fc := &fakeCheck{}
	e := checkEnv(t, fc)
	c, csrf := e.signedIn(t, "alice")
	saveConn(t, e, c, csrf, "miniflux", "https://mf.example.com", "tok-SECRET")
	if fc.count() != 1 || fc.last() != [3]string{"miniflux", "https://mf.example.com", "tok-SECRET"} {
		t.Fatalf("calls = %v", fc.calls)
	}
	_, body := e.get(t, c, "/settings/destinations")
	for _, want := range []string{`data-state="ok"`, "Connected", "Last checked", "when saved"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "tok-SECRET") {
		t.Error("secret rendered")
	}
}

func TestDestinationSaveShowsFailureReason(t *testing.T) {
	fc := &fakeCheck{fn: func(string, string, string) error {
		return fmt.Errorf("GET x: %w", &dest.StatusError{Code: 401})
	}}
	e := checkEnv(t, fc)
	c, csrf := e.signedIn(t, "alice")
	resp := saveConn(t, e, c, csrf, "miniflux", "https://mf.example.com", "tok-SECRET")
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save = %d", resp.StatusCode)
	}
	_, body := e.get(t, c, "/settings/destinations")
	for _, want := range []string{`data-state="auth"`, "Key problem", "HTTP 401"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	var n int
	_ = e.st.DB().QueryRow(`SELECT COUNT(*) FROM connections`).Scan(&n)
	if n != 1 {
		t.Error("connection not saved")
	}
}

func TestTestConnectionButton(t *testing.T) {
	fc := &fakeCheck{}
	e := checkEnv(t, fc)
	c, csrf := e.signedIn(t, "alice")
	saveConn(t, e, c, csrf, "miniflux", "https://mf.example.com", "tok")
	_, body := e.get(t, c, "/settings/destinations")
	if !strings.Contains(body, `action="/settings/destinations/miniflux/test"`) {
		t.Fatal("no Test connection form")
	}
	resp := e.post(t, c, "/settings/destinations/miniflux/test", url.Values{"csrf": {csrf}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/destinations#dest-miniflux" {
		t.Fatalf("test = %d -> %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if fc.count() != 2 {
		t.Errorf("calls = %d", fc.count())
	}
	if _, body = e.get(t, c, "/settings/destinations"); !strings.Contains(body, "by Test connection") {
		t.Error("via not shown")
	}
	if resp := e.post(t, c, "/settings/destinations/miniflux/test", url.Values{}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no csrf = %d", resp.StatusCode)
	}
	if resp := e.post(t, c, "/settings/destinations/karakeep/test", url.Values{"csrf": {csrf}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("nothing saved = %d", resp.StatusCode)
	}
	if resp := e.post(t, c, "/settings/destinations/bogus/test", url.Values{"csrf": {csrf}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bogus kind = %d", resp.StatusCode)
	}
}

func TestCheckSecretRules(t *testing.T) {
	fc := &fakeCheck{}
	e := checkEnv(t, fc)
	c, csrf := e.signedIn(t, "alice")
	saveConn(t, e, c, csrf, "metube", "https://mt.example.com", "")
	if fc.count() != 1 || fc.last() != [3]string{"metube", "https://mt.example.com", ""} {
		t.Fatalf("metube calls = %v", fc.calls)
	}
	saveConn(t, e, c, csrf, "karakeep", "https://kk.example.com", "")
	if fc.count() != 1 {
		t.Errorf("karakeep without a key was checked: %v", fc.calls)
	}
	_, body := e.get(t, c, "/settings/destinations")
	if !strings.Contains(body, `data-state="auth"`) || !strings.Contains(body, "No key is saved.") {
		t.Error("missing key not shown")
	}
}

func TestConnectionStatusIsPerUser(t *testing.T) {
	fc := &fakeCheck{fn: func(_, base, _ string) error {
		if base == "https://a.example.com" {
			return fmt.Errorf("GET x: %w", &dest.StatusError{Code: 401})
		}
		return nil
	}}
	e := checkEnv(t, fc)
	ca, csrfA := e.signedIn(t, "alice")
	cb, csrfB := e.signedIn(t, "bob")
	saveConn(t, e, ca, csrfA, "miniflux", "https://a.example.com", "ka")
	saveConn(t, e, cb, csrfB, "miniflux", "https://b.example.com", "kb")
	_, pa := e.get(t, ca, "/settings/destinations")
	_, pb := e.get(t, cb, "/settings/destinations")
	if !strings.Contains(pa, `data-state="auth"`) || strings.Contains(pa, `data-state="ok"`) {
		t.Error("alice's page wrong")
	}
	if !strings.Contains(pb, `data-state="ok"`) || strings.Contains(pb, `data-state="auth"`) {
		t.Error("bob's page wrong")
	}
	e.post(t, cb, "/settings/destinations/miniflux/test", url.Values{"csrf": {csrfB}})
	if got := fc.last(); got[1] != "https://b.example.com" {
		t.Errorf("bob's test checked %v", got)
	}
}
