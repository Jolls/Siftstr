package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Jolls/Siftstr/internal/auth"
)

func TestLoginRateLimitIs429(t *testing.T) {
	e := setup(t, nil, false)
	_, _ = e.auth.CreateUser(context.Background(), "alice", "password-one", auth.RoleUser)
	c := e.client()
	for i := 0; i < 5; i++ { // 5 is auth's per-(username, IP) limit
		resp := e.login(t, c, "alice", "wrong")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("bad login %d = %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}
	resp := e.login(t, c, "alice", "password-one")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("blocked login = %d", resp.StatusCode)
	}
	if body := e.body(t, resp); !strings.Contains(body, "try again later") {
		t.Errorf("429 page = %q", body)
	}
}
