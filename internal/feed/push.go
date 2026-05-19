package feed

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/mhd7966/vidmerce/internal/video"
)

// FeedGlobalKey is the Redis sorted-set key used by the push-mode fetcher.
// Members are video UUIDs (stringified); scores are the video's created_at
// in unix microseconds (cleanly representable as a float64 well past year 2200).
const FeedGlobalKey = "feed:global"

// PushHydrator is the slice of the video repository the push fetcher needs
// to materialise IDs into full Video records. Defining the interface here
// (consumer side) keeps the fetcher decoupled from the repo's other methods.
type PushHydrator interface {
	FindByIDs(ctx context.Context, ids []uuid.UUID) ([]video.Video, error)
	// FeedPage is also borrowed for cold-start backfill — it's already on the
	// repo because the PullFetcher uses it; we reuse it here rather than
	// duplicating the SQL.
	FeedPage(ctx context.Context, createdAt time.Time, id uuid.UUID, limit int) ([]video.Video, error)
}

// PushFetcher answers feed reads by paging over a Redis ZSET and hydrating
// the IDs through Postgres. The ZSET is populated by Publish() (called on
// each video creation in push mode) and bounded by `cap` so memory is
// predictable even with millions of videos.
type PushFetcher struct {
	rdb *goredis.Client
	src PushHydrator
	cap int
	log *slog.Logger
}

// NewPushFetcher wires the fetcher. `cap` is the maximum number of entries
// kept in the ZSET; older entries are pruned on each Publish.
func NewPushFetcher(rdb *goredis.Client, src PushHydrator, cap int, log *slog.Logger) *PushFetcher {
	if cap <= 0 {
		cap = 1000
	}
	return &PushFetcher{rdb: rdb, src: src, cap: cap, log: log}
}

// scoreFor produces the ZSET score for a video's created_at. Microsecond
// precision keeps tied-score collisions vanishingly rare in practice.
func scoreFor(t time.Time) float64 { return float64(t.UnixMicro()) }

// Publish fans-out a newly created video into the global feed cache. It is
// safe to call concurrently from many goroutines (ZADD + ZREMRANGEBYRANK are
// individually atomic; the worst case is a transient overflow above `cap`
// that's reaped on the next Publish).
//
// Called via video.WithOnCreate(...) when FEED_MODE=push.
func (p *PushFetcher) Publish(ctx context.Context, v video.Video) {
	pipe := p.rdb.Pipeline()
	pipe.ZAdd(ctx, FeedGlobalKey, goredis.Z{
		Score:  scoreFor(v.CreatedAt),
		Member: v.ID.String(),
	})
	// Keep only the top `cap` highest-scored entries. Negative indices in
	// Redis count from the tail, so we drop everything *before* the top cap.
	pipe.ZRemRangeByRank(ctx, FeedGlobalKey, 0, int64(-p.cap-1))
	if _, err := pipe.Exec(ctx); err != nil {
		p.log.Warn("feed publish failed; degraded read path will fall back to Postgres",
			slog.String("video_id", v.ID.String()),
			slog.Any("error", err),
		)
	}
}

// Warmup pre-loads the most recent videos into the ZSET. Called once at app
// start when FEED_MODE=push so the very first request after a Redis flush
// doesn't see an empty feed.
func (p *PushFetcher) Warmup(ctx context.Context) error {
	rows, err := p.src.FeedPage(ctx, time.Time{}, uuid.Nil, p.cap)
	if err != nil {
		return fmt.Errorf("warmup query: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}
	members := make([]goredis.Z, 0, len(rows))
	for _, v := range rows {
		members = append(members, goredis.Z{
			Score:  scoreFor(v.CreatedAt),
			Member: v.ID.String(),
		})
	}
	if err := p.rdb.ZAdd(ctx, FeedGlobalKey, members...).Err(); err != nil {
		return fmt.Errorf("warmup zadd: %w", err)
	}
	return nil
}

// Fetch implements Fetcher.
//
// We page by ZREVRANGEBYSCORE with the cursor's score as an exclusive upper
// bound (`(score`). Items with the exact same score as the cursor would be
// dropped this way — in practice impossible given microsecond precision, but
// we document it in edge-cases.md and the rule is "use higher precision than
// you think you need" anyway.
func (p *PushFetcher) Fetch(ctx context.Context, cursor Cursor, limit int) (Page, error) {
	if limit <= 0 {
		return Page{}, fmt.Errorf("limit must be positive")
	}
	max := "+inf"
	if !cursor.IsZero() {
		max = "(" + strconv.FormatFloat(scoreFor(cursor.CreatedAt), 'f', -1, 64)
	}

	ids, err := p.rdb.ZRevRangeByScore(ctx, FeedGlobalKey, &goredis.ZRangeBy{
		Min:    "-inf",
		Max:    max,
		Offset: 0,
		Count:  int64(limit + 1),
	}).Result()
	if err != nil {
		return Page{}, fmt.Errorf("push feed zrange: %w", err)
	}
	if len(ids) == 0 {
		return Page{}, nil
	}

	// Parse and hydrate. We hydrate ALL ids we got (including the limit+1th
	// if present) so we can compute the next cursor from its created_at.
	uuids := make([]uuid.UUID, 0, len(ids))
	for _, s := range ids {
		if id, err := uuid.Parse(s); err == nil {
			uuids = append(uuids, id)
		}
	}
	videos, err := p.src.FindByIDs(ctx, uuids)
	if err != nil {
		return Page{}, fmt.Errorf("push feed hydrate: %w", err)
	}

	// FindByIDs returns rows in arbitrary order; restore the ZSET order so the
	// client gets newest-first.
	order := make(map[uuid.UUID]int, len(uuids))
	for i, id := range uuids {
		order[id] = i
	}
	sorted := make([]video.Video, len(videos))
	copy(sorted, videos)
	sortByOrder(sorted, order)

	var nextCursor string
	if len(sorted) > limit {
		last := sorted[limit-1]
		nextCursor = Cursor{CreatedAt: last.CreatedAt, ID: last.ID}.Encode()
		sorted = sorted[:limit]
	}
	return Page{Items: sorted, NextCursor: nextCursor}, nil
}

// sortByOrder reorders `vs` in place to match the index map `order`. We avoid
// importing sort to keep the dependency surface tiny; this is an O(N log N)
// stable-ish in-place sort using a basic insertion approach (N is bounded by
// limit+1, typically <= 51).
func sortByOrder(vs []video.Video, order map[uuid.UUID]int) {
	for i := 1; i < len(vs); i++ {
		j := i
		for j > 0 && order[vs[j].ID] < order[vs[j-1].ID] {
			vs[j], vs[j-1] = vs[j-1], vs[j]
			j--
		}
	}
}
