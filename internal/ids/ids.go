// Package ids makes the random, prefixed identifiers used for database rows.
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

// New returns prefix followed by 96 random bits in hex, for example
// "itm_3f9a...". Rows are never looked up by ID alone, so IDs need to be
// unique and unguessable, not secret.
func New(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}
