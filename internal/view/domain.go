// Package view implements view tracking with a pluggable spam-filter chain
// and an async ClickHouse sink.
//
// Hot path (API):
//
//	POST /videos/:id/view  →  filter chain (watch ≥⅓ duration → duration rate)
//	                       →  unique marker (is_unique, replays still counted)
//	                       →  XADD stream:views
//	                       →  202 Accepted with {accepted: bool, rejected_by: ...}
//
// Cold path (worker):
//
//	stream:views → batch (size N or T ms) → INSERT INTO video_views (ClickHouse)
//	             → XACK the whole batch on success
//
// Read path:
//
//	GET /videos/:id/stats hits the SummingMergeTree pre-aggregate
//	`video_views_daily` (kept up to date by ClickHouse's materialized view).
//
// Design rationale:
//   - Filter chain is at the *edge*, not the worker, so spam doesn't even
//     enter the stream — saves Redis memory and worker CPU. Expensive filters
//     (ML bot detection, IP reputation) belong on the worker side; the chain
//     abstraction here makes it trivial to move them.
//   - We never mutate Postgres on the view path. Views are append-only
//     analytics, perfectly aligned with ClickHouse's strengths.
package view

import (
	"github.com/google/uuid"
)

// streamKey is the Redis Stream the worker consumes from.
const streamKey = "stream:views"

// streamMaxLen caps stream length (XADD MAXLEN ~). Sized for several minutes
// of headroom at ~10k views/sec.
const streamMaxLen = 5_000_000

// Input is the raw view as received at the HTTP edge. The handler builds this
// from the request line, body, and (optionally) auth context.
type Input struct {
	VideoID uuid.UUID
	// ViewerID is non-nil for authenticated requests; for anonymous viewers
	// the dedup / rate-limit key falls back to IPHash.
	ViewerID *uuid.UUID
	IPHash   string // 32-char digest (see Hash helpers)
	UAHash   string // 16-char digest
	Country  string // ISO-3166 alpha-2 or "" if unknown
	WatchMs      int // milliseconds of video watched (client-reported)
	DurationSec  int // from Redis cache; 0 = unknown (set by Service before chain)
}

// SubjectKey returns the bucket / dedup partition for this viewer. Logged-in
// users get their user id (so the same user across multiple devices is one
// subject); anonymous viewers fall back to their (ip_hash, ua_hash) tuple to
// avoid a single NAT'd network becoming a giant collisions bucket.
func (i Input) SubjectKey() string {
	if i.ViewerID != nil {
		return "u:" + i.ViewerID.String()
	}
	return "a:" + i.IPHash + ":" + i.UAHash
}

// Result is what the service returns to the handler. RejectedBy is empty on
// the accept path; on reject it contains the name of the filter that voted no
// (e.g. "dedup", "leaky_bucket"). The HTTP response always uses 202 — clients
// shouldn't behave differently based on whether a view was counted, since
// that's exactly the signal a spammer would use to probe the filter rules.
type Result struct {
	Accepted   bool
	RejectedBy string
	IsUnique   bool // true if first view in the unique-view window (replays may still count as total views)
}

// Event is the on-wire shape carried on stream:views and ultimately rendered
// into a ClickHouse row. We use plain strings on the wire because Redis
// Streams only carry string field/value pairs.
type Event struct {
	StreamID  string
	VideoID   uuid.UUID
	ViewerID  *uuid.UUID // nil for anonymous
	IPHash    string
	UAHash    string
	Country   string
	IsUnique  bool
	EventTime int64 // unix milliseconds
}
