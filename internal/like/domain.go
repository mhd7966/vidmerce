// Package like implements the asynchronous like / unlike pipeline.
//
// Hot path (API):
//
//	POST /videos/:id/like   →  atomic Redis Lua script that
//	                              1. checks/updates the user's liked-videos set,
//	                              2. increments/decrements the per-video counter,
//	                              3. XADDs an event onto stream:likes,
//	                            then returns 202 immediately with the new state.
//
// Cold path (worker):
//
//	stream:likes → filter chain → Postgres TX that maintains the *exact*
//	likes_count in video_stats inside the same transaction as the edge change.
//
// Read paths:
//
//	"did I like this video?"  →  SISMEMBER on user:{uid}:liked:videos
//	"how many likes?"          →  Redis counter (fast), Postgres video_stats.likes_count (exact).
//
// A periodic reconciler (Reconciler) scans Postgres for any drift between
// the denormalised likes_count and the actual COUNT(*) and corrects it. Under
// correct code that scan never finds anything; if it does, we have a bug and
// want to know about it immediately.
package like

import (
	"errors"

	"github.com/google/uuid"
)

// ErrInvalidVideoID is returned when the path :id can't be parsed.
var ErrInvalidVideoID = errors.New("invalid video id")

// State is the snapshot returned to the API caller. Liked == is this user
// liked? Count == current global count for the video (from Redis, eventual).
type State struct {
	Liked bool  `json:"liked"`
	Count int64 `json:"count"`
}

// Op is the discriminator carried in the stream entry. We use a string (not
// an int / enum) because it's grep-able on the wire and resilient to
// reordering of constant definitions.
type Op string

const (
	OpLike   Op = "like"
	OpUnlike Op = "unlike"
)

// Event is the unit of work flowing through stream:likes. The worker reads
// it, runs filters, and applies it to Postgres.
type Event struct {
	UserID  uuid.UUID
	VideoID uuid.UUID
	Op      Op
	// StreamID is the Redis Stream entry id (e.g. "1684395820123-0"). Carried
	// alongside the event so the worker can XACK after a successful apply.
	StreamID string
}

// Result describes the outcome of applying a single event to Postgres. It is
// used by tests and by the worker's logging, never serialised on the wire.
type Result struct {
	Changed   bool  // true if Postgres state actually changed (edge inserted/deleted)
	NewCount  int64 // value of video_stats.likes_count after apply
	WasNoop   bool  // true if event was a duplicate (e.g. like on already-liked)
}
