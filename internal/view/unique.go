package view

import (
	"context"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// UniqueMarker records first-view-in-window for analytics (is_unique), without
// rejecting replay events.
type UniqueMarker struct {
	rdb *goredis.Client
	ttl time.Duration
}

// NewUniqueMarker constructs a marker. ttl is the unique-view window
// (e.g. 10m or 24h).
func NewUniqueMarker(rdb *goredis.Client, ttl time.Duration) *UniqueMarker {
	return &UniqueMarker{rdb: rdb, ttl: ttl}
}

func uniqueKey(in Input) string {
	return "view:unique:" + in.SubjectKey() + ":" + in.VideoID.String()
}

// TryMark returns true if this is the first view in the TTL window for this
// (subject, video). Replays return false but are still valid total views.
func (m *UniqueMarker) TryMark(ctx context.Context, in Input) (bool, error) {
	ok, err := m.rdb.SetNX(ctx, uniqueKey(in), 1, m.ttl).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}
