package metube

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jolls/Siftstr/internal/dest"
)

func TestCheckReadsHistoryOnly(t *testing.T) {
	var calls []string
	var auth string
	reply := `{"done":[],"queue":[],"pending":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	if err := New(srv.URL, "", nil).Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "GET /history" || auth != "" {
		t.Fatalf("%v %q", calls, auth)
	}
	if err := New(srv.URL, "s3", nil).Check(context.Background()); err != nil || auth != "Bearer s3" {
		t.Fatalf("%v %q", err, auth)
	}
	reply = `{"x":1}`
	if err := New(srv.URL, "", nil).Check(context.Background()); !errors.Is(err, dest.ErrUnexpectedReply) {
		t.Errorf("wrong shape: %v", err)
	}
	for _, c := range calls {
		if c != "GET /history" {
			t.Errorf("unexpected call %s", c)
		}
	}
}
