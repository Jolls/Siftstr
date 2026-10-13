package miniflux

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Jolls/Siftstr/internal/dest"
)

func TestCheckCallsMeWithToken(t *testing.T) {
	var method, path, token string
	reply, code := `{"id":1}`, 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, token = r.Method, r.URL.Path, r.Header.Get("X-Auth-Token")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	d := New(srv.URL, "tok", nil)
	if err := d.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if method != "GET" || path != "/v1/me" || token != "tok" {
		t.Fatalf("%s %s %q", method, path, token)
	}
	reply = `{}`
	if err := d.Check(context.Background()); !errors.Is(err, dest.ErrUnexpectedReply) {
		t.Errorf("empty user: %v", err)
	}
	reply, code = `{}`, 401
	var se *dest.StatusError
	if err := d.Check(context.Background()); !errors.As(err, &se) || se.Code != 401 {
		t.Errorf("401: %v", err)
	}
}
