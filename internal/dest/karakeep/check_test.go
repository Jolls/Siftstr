package karakeep

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jolls/Siftstr/internal/dest"
)

func TestClientCheckCallsUsersMe(t *testing.T) {
	var method, path, auth string
	reply := `{"id":"u"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "k", nil)
	if err := c.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if method != "GET" || path != "/api/v1/users/me" || auth != "Bearer k" {
		t.Fatalf("%s %s %q", method, path, auth)
	}
	reply = `{}`
	if err := c.Check(context.Background()); !errors.Is(err, dest.ErrUnexpectedReply) {
		t.Errorf("empty user: %v", err)
	}
	if err := New(srv.URL, "k", nil).Check(context.Background()); !errors.Is(err, dest.ErrUnexpectedReply) {
		t.Errorf("Destination.Check: %v", err)
	}
}
