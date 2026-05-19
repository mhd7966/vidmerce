// Package feed renders the global video feed. It supports two implementations
// chosen at startup via the FEED_MODE environment variable:
//
//	pull  — keyset pagination over the videos table in Postgres. Latency
//	        grows with the per-query work, not with how far you've scrolled.
//	        Simple, correct, and a good default up to ~10k QPS.
//
//	push  — videos are fan-out-on-write into a Redis sorted set (feed:global).
//	        Reads are O(log N + page) hits against Redis plus one batched
//	        Postgres hydration for the page. Trades write amplification for
//	        read latency. Without a follow graph it's a *global* feed cache;
//	        personalisation (per-user ZSETs) is described in docs/architecture.md.
//
// Both modes share the same cursor wire format so clients don't care which
// mode the server is running.
package feed

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/video"
)

// ErrInvalidCursor is returned for malformed cursor strings. Distinct from a
// "not found" / empty page so the handler can map it to 400.
var ErrInvalidCursor = errors.New("invalid cursor")

// Cursor encodes the keyset boundary. Field names are short to keep the
// base64 payload compact on the wire (Cursor strings travel on every page).
type Cursor struct {
	CreatedAt time.Time `json:"t"`
	ID        uuid.UUID `json:"i"`
}

// Encode produces the opaque string that clients hand back on the next page.
func (c Cursor) Encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor reverses Encode. Empty string is treated as "no cursor" (start
// of feed) and returns the zero value with a nil error.
func DecodeCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	if c.ID == uuid.Nil || c.CreatedAt.IsZero() {
		return Cursor{}, ErrInvalidCursor
	}
	return c, nil
}

// IsZero reports whether this cursor is the "start of feed" sentinel.
func (c Cursor) IsZero() bool { return c.ID == uuid.Nil && c.CreatedAt.IsZero() }

// Page is the result of a single Fetch call. NextCursor is non-empty exactly
// when there is at least one more video after Items.
type Page struct {
	Items      []video.Video
	NextCursor string
}
