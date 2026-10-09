package main

import (
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
