// Package util holds small dependency-free helpers shared across omnigate.
package util

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
)

// UUID returns a random RFC-4122-v4-looking hex string (no dashes), used for
// chat ids and tool-call ids. It does not need to be a real UUID.
func UUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; fall back to a fixed pattern.
		return "00000000000000000000000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return hex.EncodeToString(b[:])
}

// SHA1Hex returns the lowercase hex SHA-1 of s.
func SHA1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NewCallID returns an OpenAI-style tool call id.
func NewCallID() string {
	return "call_" + UUID()[:22]
}

// Clip truncates s to n runes, appending a marker when truncated.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// Collapse collapses all runs of whitespace into single spaces and trims.
func Collapse(s string) string {
	out := make([]rune, 0, len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' || r == '\v' {
			space = true
			continue
		}
		if space && len(out) > 0 {
			out = append(out, ' ')
		}
		space = false
		out = append(out, r)
	}
	return string(out)
}

// Quote is a small helper for building JSON error strings without importing
// encoding/json at the call site.
func Quote(s string) string { return fmt.Sprintf("%q", s) }
