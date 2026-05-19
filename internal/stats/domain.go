// Package stats serves the analytics endpoint for a single video.
//
//	GET /videos/:id/stats  →  { views, unique_views, likes, engagement_rate }
//
// The endpoint is a hot read path with a wide fan-out (every video page load
// can request it). It reads from two data stores:
//
//   - ClickHouse `video_views_daily` for view counts (SummingMergeTree
//     pre-aggregate, kept current by the materialized view from Step 7).
//   - Postgres `video_stats.likes_count` for the exact like count
//     (maintained by the like worker in Step 6).
//
// To keep both backends safe under load, the service applies three layers
// of stampede protection:
//
//  1. Redis cache (TTL = STATS_CACHE_TTL): default 30s.
//  2. In-process singleflight: coalesces concurrent in-process misses.
//  3. Distributed Redis lock (`lock:stats:{video_id}`, TTL = STATS_LOCK_TTL):
//     across replicas, only one wins the recompute; losers briefly re-poll
//     the cache, then compute anyway as a safety net.
//
// `engagement_rate` is computed as `likes / unique_views`. We chose unique
// views over total views as the denominator because total counts replays
// inside the dedup TTL, which would deflate the rate for the same audience
// re-watching. Documented in docs/edge-cases.md.
package stats

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrVideoNotFound is returned by Service.Get when the requested video does
// not exist. Handlers map this to 404.
var ErrVideoNotFound = errors.New("video not found")

// Stats is the payload of GET /videos/:id/stats. ComputedAt is exposed so
// clients can see how fresh the value is — useful for "data as of HH:MM"
// labels on a creator dashboard.
type Stats struct {
	VideoID        uuid.UUID `json:"video_id"`
	Views          int64     `json:"views"`
	UniqueViews    int64     `json:"unique_views"`
	Likes          int64     `json:"likes"`
	EngagementRate float64   `json:"engagement_rate"`
	ComputedAt     time.Time `json:"computed_at"`
}

// engagementRate computes likes / unique_views with a divide-by-zero guard.
// Returns 0 when there's no audience to engage with — defined as the polite
// default for empty videos.
func engagementRate(likes, uniqueViews int64) float64 {
	if uniqueViews <= 0 {
		return 0
	}
	return float64(likes) / float64(uniqueViews)
}
