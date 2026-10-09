package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func render(t *testing.T, name string, p Page) string {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.Render(rec, http.StatusOK, name, p)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	return rec.Body.String()
}

func TestLoginPage(t *testing.T) {
	body := render(t, "login", Page{Title: "Sign in", CSRF: "tok123", Error: "bad login"})
	for _, want := range []string{`name="csrf" value="tok123"`, `autocomplete="current-password"`, "bad login"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, `href="/today"`) {
		t.Error("logged-out page must not show the nav")
	}
}

func TestItemEscaping(t *testing.T) {
	body := render(t, "items", Page{
		User: &User{Name: "a"}, Active: "today", Heading: "Today",
		Items: []Item{{ID: "itm_1", Title: `<script>alert(1)</script>`, URL: "https://example.com/x", State: "light"}},
	})
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("title not escaped")
	}
}

// Actioned items stay in the DOM with a badge; the badge slot always exists.
func TestItemBadgeAndControls(t *testing.T) {
	body := render(t, "items", Page{
		User: &User{Name: "a"}, Active: "today", Heading: "Today",
		Items: []Item{
			{ID: "itm_1", Title: "A", URL: "https://example.com/a", State: "archived", Badge: "archived", CanPromote: true},
			{ID: "itm_2", Title: "B", URL: "https://example.com/b", State: "light"},
		},
	})
	for _, want := range []string{
		`id="itm_1"`, `data-state="archived"`, `>archived</span>`,
		`id="itm_2"`, `data-badge hidden`, `data-action="archive"`, `data-action="keep"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Count(body, `data-action="promote"`) != 1 {
		t.Error("promote button should appear only where CanPromote is set")
	}
	if i := strings.Index(body, `<section class="items"`); i < 0 || strings.Contains(body[i:], "<form") {
		t.Error("triage controls must not be forms or direct requests")
	}
}

func TestEmptyList(t *testing.T) {
	body := render(t, "items", Page{User: &User{}, Heading: "Backlog", Empty: "Nothing carried over."})
	if !strings.Contains(body, "Nothing carried over.") {
		t.Error("empty message missing")
	}
}

func TestStatic(t *testing.T) {
	h, err := Static()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/static/pico.min.css", "/static/app.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s = %d", p, rec.Code)
		}
	}
}
