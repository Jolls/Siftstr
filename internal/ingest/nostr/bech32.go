package nostr

import (
	"encoding/hex"
	"errors"
	"strings"
)

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var errBech32 = errors.New("invalid bech32 string")

func polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := range gen {
			if top>>uint(i)&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

// convertBits regroups values from `from`-bit to `to`-bit groups.
func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc, bits uint
	var out []byte
	maxv := uint(1)<<to - 1
	for _, v := range data {
		if uint(v)>>from != 0 {
			return nil, errBech32
		}
		acc = acc<<from | uint(v)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxv))
		}
	} else if bits >= from || acc<<(to-bits)&maxv != 0 {
		return nil, errBech32
	}
	return out, nil
}

// encode makes a bech32 string from a prefix and 32 raw bytes.
func encode(hrp string, data []byte) (string, error) {
	five, err := convertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	values := append(hrpExpand(hrp), five...)
	values = append(values, 0, 0, 0, 0, 0, 0)
	mod := polymod(values) ^ 1
	var b strings.Builder
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, v := range five {
		b.WriteByte(charset[v])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(charset[mod>>uint(5*(5-i))&31])
	}
	return b.String(), nil
}

// decode returns the prefix and raw bytes of a bech32 string.
func decode(s string) (string, []byte, error) {
	if s != strings.ToLower(s) && s != strings.ToUpper(s) {
		return "", nil, errBech32
	}
	s = strings.ToLower(s)
	sep := strings.LastIndexByte(s, '1')
	if sep < 1 || len(s)-sep-1 < 6 {
		return "", nil, errBech32
	}
	hrp := s[:sep]
	values := make([]byte, 0, len(s)-sep-1)
	for _, c := range s[sep+1:] {
		i := strings.IndexRune(charset, c)
		if i < 0 {
			return "", nil, errBech32
		}
		values = append(values, byte(i))
	}
	if polymod(append(hrpExpand(hrp), values...)) != 1 {
		return "", nil, errBech32
	}
	raw, err := convertBits(values[:len(values)-6], 5, 8, false)
	if err != nil {
		return "", nil, err
	}
	return hrp, raw, nil
}

// ErrBadNpub is returned for text that is not a valid npub or hex public key.
var ErrBadNpub = errors.New("not a valid npub")

// ParsePubKey accepts an npub or a 64-character hex public key and returns
// the lowercase hex key.
func ParsePubKey(s string) (string, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "nostr:"))
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
			return strings.ToLower(s), nil
		}
		return "", ErrBadNpub
	}
	hrp, raw, err := decode(s)
	if err != nil || hrp != "npub" || len(raw) != 32 {
		return "", ErrBadNpub
	}
	return hex.EncodeToString(raw), nil
}

// Npub encodes a hex public key as an npub.
func Npub(pubHex string) string {
	raw, err := hex.DecodeString(pubHex)
	if err != nil || len(raw) != 32 {
		return pubHex
	}
	s, err := encode("npub", raw)
	if err != nil {
		return pubHex
	}
	return s
}

// Note encodes a hex event ID as a note identifier.
func Note(idHex string) string {
	raw, err := hex.DecodeString(idHex)
	if err != nil || len(raw) != 32 {
		return idHex
	}
	s, err := encode("note", raw)
	if err != nil {
		return idHex
	}
	return s
}
