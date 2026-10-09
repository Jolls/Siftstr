package secret

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGeneratesAndReuses(t *testing.T) {
	dir := t.TempDir()
	a, err := Load(dir, "")
	if err != nil || len(a) != 32 {
		t.Fatalf("first load: %v", err)
	}
	b, err := Load(dir, "")
	if err != nil || !bytes.Equal(a, b) {
		t.Fatalf("second load differs: %v", err)
	}
}

func TestLoadEnvKeyWins(t *testing.T) {
	dir := t.TempDir()
	a, _ := Load(dir, "passphrase")
	b, _ := Load(dir, "passphrase")
	c, _ := Load(dir, "other")
	if !bytes.Equal(a, b) || bytes.Equal(a, c) {
		t.Fatal("env key derivation is not stable and distinct")
	}
	if _, err := os.Stat(filepath.Join(dir, keyFile)); err == nil {
		t.Fatal("secret.key must not be written when the env key is set")
	}
}

func TestLoadRejectsBadFile(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, keyFile), []byte("nothex"), 0o600)
	if _, err := Load(dir, ""); err == nil {
		t.Fatal("expected error")
	}
}

func TestDerive(t *testing.T) {
	m := bytes.Repeat([]byte{1}, 32)
	if bytes.Equal(Derive(m, "csrf"), Derive(m, "connections")) {
		t.Fatal("purposes must yield different keys")
	}
}
