package view

import (
	"crypto/sha256"
	"encoding/hex"
)

// HashIP returns a 32-byte ASCII digest of an IP address. Truncating SHA-256
// to 32 hex chars (16 bytes of entropy) is enough to make rainbow tables
// useless while keeping the column size predictable for ClickHouse
// FixedString(32). We do NOT salt: this is for grouping not authentication,
// and a salt would defeat cross-request correlation that's needed for spam
// detection.
func HashIP(ip string) string {
	if ip == "" {
		// All-zero hash for the unknown case. Distinct from any real IP.
		return "00000000000000000000000000000000"
	}
	sum := sha256.Sum256([]byte(ip))
	return hex.EncodeToString(sum[:])[:32]
}

// HashUA returns a 16-byte ASCII digest of a User-Agent string. Same
// rationale as HashIP but narrower: UA strings have lower entropy and we
// only need them to distinguish browser fingerprints.
func HashUA(ua string) string {
	if ua == "" {
		return "0000000000000000"
	}
	sum := sha256.Sum256([]byte(ua))
	return hex.EncodeToString(sum[:])[:16]
}
