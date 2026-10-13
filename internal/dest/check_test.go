package dest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallStatusErrorTextIsStable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/x":
			http.Error(w, "no", 503)
		case "/bad":
			http.Error(w, "no", 400)
		default:
			http.Redirect(w, r, "/elsewhere", http.StatusMovedPermanently)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	_, err := Call(ctx, DefaultClient(), http.MethodGet, srv.URL+"/x?token=abc", nil, nil, nil)
	if err == nil || err.Error() != "GET "+srv.URL+"/x: status 503" {
		t.Fatalf("503: %v", err)
	}
	if p, ok := err.(interface{ Permanent() bool }); ok && p.Permanent() {
		t.Error("503 is permanent")
	}
	_, err = Call(ctx, DefaultClient(), http.MethodGet, srv.URL+"/bad", nil, nil, nil)
	if err == nil || err.Error() != "GET "+srv.URL+"/bad: status 400" {
		t.Fatalf("400: %v", err)
	}
	_, err = Call(ctx, DefaultClient(), http.MethodGet, srv.URL+"/old", nil, nil, nil)
	if err == nil || err.Error() != "GET "+srv.URL+"/old: status 301 (redirect: check that the saved base URL is the final address, e.g. https)" {
		t.Fatalf("301: %v", err)
	}
}

func TestStatusErrorIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			http.Error(w, "no", 400)
			return
		}
		http.Error(w, "no", 401)
	}))
	defer srv.Close()
	for path, want := range map[string]int{"/x": 401, "/bad": 400} {
		_, err := Call(context.Background(), DefaultClient(), http.MethodGet, srv.URL+path, nil, nil, nil)
		var se *StatusError
		if !errors.As(err, &se) || se.Code != want {
			t.Errorf("%s: %v", path, err)
		}
	}
}

type noCheck struct{}

func (noCheck) Name() string                     { return "x" }
func (noCheck) Send(context.Context, Item) error { return nil }

func TestCheckFuncUnknownKindAndNoChecker(t *testing.T) {
	f := CheckFunc(map[string]Factory{
		"plain": func(string, string, json.RawMessage) Destination { return noCheck{} },
	})
	if err := f(context.Background(), "nostr", "", ""); !errors.Is(err, ErrNoCheck) {
		t.Errorf("unknown kind: %v", err)
	}
	if err := f(context.Background(), "plain", "", ""); !errors.Is(err, ErrNoCheck) {
		t.Errorf("no Check: %v", err)
	}
}
