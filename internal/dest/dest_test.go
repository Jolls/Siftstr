package dest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedirectIsAnErrorNotASilentGet(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	_, err := Call(context.Background(), DefaultClient(), http.MethodPost, srv.URL+"/old", nil, map[string]string{"a": "b"}, nil)
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v", err)
	}
	if len(methods) != 1 {
		t.Fatalf("redirect was followed: %v", methods)
	}
}

func TestRedactDropsCredentialsAndQuery(t *testing.T) {
	got := redact("https://user:pw@host.example.com/api/x?token=abc")
	if strings.Contains(got, "pw") || strings.Contains(got, "user") || strings.Contains(got, "abc") {
		t.Fatalf("redact = %q", got)
	}
}
