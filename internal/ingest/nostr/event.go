// Package nostr ingests notes and articles from the npubs a user follows.
//
// It is a small client, not a general Nostr library: bech32 for npub and note
// identifiers, the NIP-01 event ID and Schnorr check, and a one-shot
// subscription that reads until the relay says it has sent everything.
package nostr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// Event kinds Siftstr reads.
const (
	KindProfile = 0     // NIP-01 metadata, used for the source name
	KindNote    = 1     // short text note
	KindArticle = 30023 // NIP-23 long-form content
)

// Event is a NIP-01 event.
type Event struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// ErrBadEvent means an event's ID or signature does not check out.
var ErrBadEvent = errors.New("invalid nostr event")

// ComputeID returns the event ID: the SHA-256 of the NIP-01 serialization.
func (e Event) ComputeID() string {
	var b strings.Builder
	b.WriteString(`[0,"`)
	b.WriteString(e.PubKey)
	b.WriteString(`",`)
	b.WriteString(strconv.FormatInt(e.CreatedAt, 10))
	b.WriteByte(',')
	b.WriteString(strconv.Itoa(e.Kind))
	b.WriteString(`,[`)
	for i, tag := range e.Tags {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('[')
		for j, s := range tag {
			if j > 0 {
				b.WriteByte(',')
			}
			writeString(&b, s)
		}
		b.WriteByte(']')
	}
	b.WriteString(`],`)
	writeString(&b, e.Content)
	b.WriteByte(']')
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// writeString writes s as a JSON string with only the escapes NIP-01 allows.
// encoding/json would also escape <, > and & and other control characters,
// which changes the hash.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

// Verify checks the event ID and the Schnorr signature over it.
func (e Event) Verify() error {
	if e.ComputeID() != e.ID {
		return ErrBadEvent
	}
	pk, err := hex.DecodeString(e.PubKey)
	if err != nil || len(pk) != 32 {
		return ErrBadEvent
	}
	sigBytes, err := hex.DecodeString(e.Sig)
	if err != nil || len(sigBytes) != 64 {
		return ErrBadEvent
	}
	id, err := hex.DecodeString(e.ID)
	if err != nil {
		return ErrBadEvent
	}
	pub, err := schnorr.ParsePubKey(pk)
	if err != nil {
		return ErrBadEvent
	}
	sig, err := schnorr.ParseSignature(sigBytes)
	if err != nil || !sig.Verify(id, pub) {
		return ErrBadEvent
	}
	return nil
}

// Tag returns the first value of the first tag with this name.
func (e Event) Tag(name string) string {
	for _, t := range e.Tags {
		if len(t) >= 2 && t[0] == name {
			return t[1]
		}
	}
	return ""
}

// IsReply reports whether the event answers another one. Replies are
// conversation, not something to triage.
func (e Event) IsReply() bool {
	for _, t := range e.Tags {
		if len(t) >= 2 && t[0] == "e" {
			return true
		}
	}
	return false
}

// profileName reads the display name out of a kind 0 event's content.
func profileName(content string) string {
	var p struct {
		DisplayName string `json:"display_name"`
		Name        string `json:"name"`
	}
	if json.Unmarshal([]byte(content), &p) != nil {
		return ""
	}
	if n := strings.TrimSpace(p.DisplayName); n != "" {
		return n
	}
	return strings.TrimSpace(p.Name)
}
