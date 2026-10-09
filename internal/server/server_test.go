package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := New()
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	if rec := get(t, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestLogin(t *testing.T) {
	rec := get(t, "/login")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="password"`) {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("missing CSP")
	}
}

func TestStaticAndNotFound(t *testing.T) {
	if rec := get(t, "/static/app.css"); rec.Code != http.StatusOK {
		t.Errorf("static = %d", rec.Code)
	}
	if rec := get(t, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown = %d", rec.Code)
	}
}
