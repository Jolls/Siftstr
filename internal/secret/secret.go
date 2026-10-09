// Package secret loads the instance master key and derives purpose-specific
// subkeys from it.
package secret

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const keyFile = "secret.key"

// Load returns the 32-byte master key. If envKey (SIFTSTR_SECRET_KEY) is set,
// the key is derived from it. Otherwise it is read from dataDir/secret.key,
// which is generated on first start.
func Load(dataDir, envKey string) ([]byte, error) {
	if envKey != "" {
		sum := sha256.Sum256([]byte(envKey))
		return sum[:], nil
	}
	path := filepath.Join(dataDir, keyFile)
	raw, err := os.ReadFile(path)
	if err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is not a 32-byte hex key", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// O_EXCL so two starting processes cannot overwrite each other's key.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return Load(dataDir, "")
		}
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return key, nil
}

// Derive returns a 32-byte subkey for the named purpose.
func Derive(master []byte, purpose string) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte(purpose))
	return m.Sum(nil)
}
