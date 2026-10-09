package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadPassword(t *testing.T) {
	for in, want := range map[string]string{
		"secret-pass\n":   "secret-pass",
		"secret-pass\r\n": "secret-pass",
		"secret-pass":     "secret-pass",
	} {
		got, err := readPassword(strings.NewReader(in))
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v", in, got, err)
		}
	}
	if _, err := readPassword(strings.NewReader("")); err == nil {
		t.Error("empty stdin should error")
	}
}

// The CLI creates users in the same database the server uses, and the
// password is never taken from arguments.
func TestUserAddAndSetPassword(t *testing.T) {
	t.Setenv("SIFTSTR_DATA_DIR", t.TempDir())
	if err := run([]string{"user", "add", "--admin", "alice"}, strings.NewReader("password-one\n")); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"user", "add", "alice"}, strings.NewReader("password-two\n")); err == nil {
		t.Error("duplicate user accepted")
	}
	if err := run([]string{"user", "set-password", "alice"}, strings.NewReader("password-three\n")); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"user", "set-password", "ghost"}, strings.NewReader("password-three\n")); err == nil {
		t.Error("unknown user accepted")
	}
	if err := run([]string{"user", "add", "bob"}, strings.NewReader("short\n")); err == nil {
		t.Error("weak password accepted")
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer bad.Close()

	if err := healthcheck(strings.TrimPrefix(ok.URL, "http://")); err != nil {
		t.Errorf("healthy server: %v", err)
	}
	if err := healthcheck(strings.TrimPrefix(bad.URL, "http://")); err == nil {
		t.Error("503 should fail the check")
	}
	if err := healthcheck("not-a-listen-address"); err == nil {
		t.Error("bad address should fail")
	}
}

func TestAPIKeyCLI(t *testing.T) {
	t.Setenv("SIFTSTR_DATA_DIR", t.TempDir())
	if err := run([]string{"user", "add", "alice"}, strings.NewReader("password-one\n")); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := apikeyCmd([]string{"create", "alice", "--label", "claude"}, &out); err != nil {
		t.Fatal(err)
	}
	var id, secret string
	for _, line := range strings.Split(out.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "key id: "); ok {
			id = v
		}
		if strings.HasPrefix(line, "sft_") {
			secret = line
		}
	}
	if id == "" || secret == "" {
		t.Fatalf("create output = %q", out.String())
	}

	out.Reset()
	if err := apikeyCmd([]string{"list", "alice"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), id) || !strings.Contains(out.String(), "active") || strings.Contains(out.String(), secret) {
		t.Fatalf("list must show the key id and state but never the secret: %q", out.String())
	}

	if err := apikeyCmd([]string{"revoke", "alice", id}, &out); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	_ = apikeyCmd([]string{"list", "alice"}, &out)
	if !strings.Contains(out.String(), "revoked") {
		t.Fatalf("after revoke: %q", out.String())
	}
	if err := apikeyCmd([]string{"create", "ghost"}, &out); err == nil {
		t.Error("unknown user accepted")
	}
}
