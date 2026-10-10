package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// stopped inserts a stopped (paused) outbox entry for a user.
func (e *env) stopped(t *testing.T, userID, id string) {
	t.Helper()
	e.exec(t, `INSERT INTO outbox (id, user_id, op, payload, status, attempts, last_error, created_at, next_attempt_at)
		VALUES (?, ?, 'karakeep', '{"id":"i1","title":"A kept article"}', 'failed', 10, 'GET x: status 503', '2026-10-09T08:00:00Z', '')`, id, userID)
}

func (e *env) userID(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := e.st.DB().QueryRow(`SELECT id FROM users WHERE username = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStoppedUpdatesRaiseAnAlertOnEveryPage(t *testing.T) {
	e := setup(t, nil, false)
	c, _ := e.signedIn(t, "alice")
	get := func(path string) string {
		resp, err := c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		return e.body(t, resp)
	}
	if strings.Contains(get("/today"), "could not be sent") {
		t.Fatal("alert shown with nothing wrong")
	}
	e.stopped(t, e.userID(t, "alice"), "obx_1")
	for _, path := range []string{"/today", "/backlog", "/read", "/settings/keys"} {
		if body := get(path); !strings.Contains(body, "1 update to your other services could not be sent") {
			t.Errorf("%s has no alert", path)
		}
	}
}

func TestActivityListsRetriesAndDismisses(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	e.stopped(t, e.userID(t, "alice"), "obx_1")

	resp, _ := c.Get(e.srv.URL + "/settings/activity")
	page := e.body(t, resp)
	for _, want := range []string{"A kept article", "paused", "status 503", "Retry", "Dismiss"} {
		if !strings.Contains(page, want) {
			t.Errorf("activity page lacks %q", want)
		}
	}

	if r := e.post(t, c, "/settings/activity/obx_1/retry", url.Values{"csrf": {csrf}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("retry = %d", r.StatusCode)
	}
	var status string
	_ = e.st.DB().QueryRow(`SELECT status FROM outbox WHERE id = 'obx_1'`).Scan(&status)
	if status != "pending" {
		t.Fatalf("status after retry = %s", status)
	}

	e.exec(t, `UPDATE outbox SET status = 'failed' WHERE id = 'obx_1'`)
	if r := e.post(t, c, "/settings/activity/obx_1/dismiss", url.Values{"csrf": {csrf}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("dismiss = %d", r.StatusCode)
	}
	resp, _ = c.Get(e.srv.URL + "/settings/activity")
	if body := e.body(t, resp); strings.Contains(body, "A kept article") || strings.Contains(body, "could not be sent") {
		t.Error("dismissed entry or its alert is still shown")
	}
}

func TestActivityNeedsCSRFAndIsPerUser(t *testing.T) {
	e := setup(t, nil, false)
	c, csrf := e.signedIn(t, "alice")
	bc, bcsrf := e.signedIn(t, "bob")
	e.stopped(t, e.userID(t, "alice"), "obx_1")

	if r := e.post(t, c, "/settings/activity/obx_1/retry", url.Values{}); r.StatusCode != http.StatusForbidden {
		t.Errorf("retry without CSRF = %d", r.StatusCode)
	}
	if r := e.post(t, bc, "/settings/activity/obx_1/retry", url.Values{"csrf": {bcsrf}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("bob retrying alice's entry = %d, want 404", r.StatusCode)
	}
	if r := e.post(t, bc, "/settings/activity/obx_1/dismiss", url.Values{"csrf": {bcsrf}}); r.StatusCode != http.StatusNotFound {
		t.Errorf("bob dismissing alice's entry = %d, want 404", r.StatusCode)
	}
	resp, _ := bc.Get(e.srv.URL + "/settings/activity")
	if body := e.body(t, resp); strings.Contains(body, "A kept article") || strings.Contains(body, "could not be sent") {
		t.Error("bob sees alice's stopped update")
	}
	_ = csrf
}
