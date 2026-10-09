package miniflux

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jolls/Siftstr/internal/dest"
)

func TestSendMarksEntriesRead(t *testing.T) {
	var got map[string]any
	var token, method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, method, path = r.Header.Get("X-Auth-Token"), r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	d := New(srv.URL, "tok", nil)
	for i := 0; i < 2; i++ { // marking read twice is harmless
		if err := d.Send(context.Background(), dest.Item{ID: "i", EntryIDs: []int64{7, 8}}); err != nil {
			t.Fatal(err)
		}
	}
	if token != "tok" || method != "PUT" || path != "/v1/entries" || got["status"] != "read" {
		t.Fatalf("%s %s %s %v", token, method, path, got)
	}
	if ids := got["entry_ids"].([]any); len(ids) != 2 || ids[0].(float64) != 7 {
		t.Fatalf("%v", got)
	}
}

func TestNoEntriesMakesNoCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected call") }))
	defer srv.Close()
	if err := New(srv.URL, "t", nil).Send(context.Background(), dest.Item{ID: "i"}); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedTokenIsRetriedNotPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 401) }))
	defer srv.Close()
	err := New(srv.URL, "bad", nil).Send(context.Background(), dest.Item{ID: "i", EntryIDs: []int64{1}})
	if err == nil {
		t.Fatal("want error")
	}
	if p, ok := err.(interface{ Permanent() bool }); ok && p.Permanent() {
		t.Fatal("a 401 should retry so the user can fix the token")
	}
}
