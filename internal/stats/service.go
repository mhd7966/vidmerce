package stats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/mhd7966/vidmerce/internal/platform/metrics"
)

// LikesCounter is the slice of like.Repository the stats service depends on.
// Defined here on the consumer side (interface segregation): the service
// doesn't care about Apply / Reconcile / Sample, only Count.
type LikesCounter interface {
	Count(ctx context.Context, videoID uuid.UUID) (int64, error)
}

// VideoExister is the minimum slice of the video service the stats service
// uses to verify a video actually exists before computing stats. Avoids the
// circular import that would happen if we depended on the full *video.Service.
type VideoExister interface {
	Exists(ctx context.Context, videoID uuid.UUID) (bool, error)
}

// Service is the read-side of the analytics path.
//
// Failure modes and their handling:
//
//   - Redis down (cache GET fails): fall through to compute. Slow but correct.
//   - Distributed lock acquire fails: log + compute anyway. Worst case we
//     do N concurrent computes briefly, which is still cheaper than a 5xx.
//   - ClickHouse times out: return ErrCompute. Handler maps to 503.
//   - Postgres times out: same as above.
//
// We never serve a partially-computed value (i.e. likes without views). The
// service is conservative: either fresh-and-complete or a clearly-labelled
// failure.
type Service struct {
	views ViewsCounter
	likes LikesCounter
	video VideoExister
	rdb   *goredis.Client
	log   *slog.Logger
	sf    singleflight.Group

	cacheTTL  time.Duration
	lockTTL   time.Duration
	lockRetry time.Duration
	clock     func() time.Time
}

// Config bundles the tunables. Zero values fall back to sensible production
// defaults so callers can pass {} during tests.
type Config struct {
	CacheTTL  time.Duration
	LockTTL   time.Duration
	LockRetry time.Duration
}

// NewService wires the service. Logger is required for visibility; the rest
// fall back to defaults if zero-valued.
func NewService(
	views ViewsCounter,
	likes LikesCounter,
	video VideoExister,
	rdb *goredis.Client,
	log *slog.Logger,
	cfg Config,
) *Service {
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 30 * time.Second
	}
	if cfg.LockTTL <= 0 {
		cfg.LockTTL = 5 * time.Second
	}
	if cfg.LockRetry <= 0 {
		cfg.LockRetry = 75 * time.Millisecond
	}
	return &Service{
		views:     views,
		likes:     likes,
		video:     video,
		rdb:       rdb,
		log:       log,
		cacheTTL:  cfg.CacheTTL,
		lockTTL:   cfg.LockTTL,
		lockRetry: cfg.LockRetry,
		clock:     time.Now,
	}
}

// cacheKey / lockKey are the only place these patterns appear; keep them
// together so the namespaces never drift from docs/redis-keys.md.
func cacheKey(vid uuid.UUID) string { return "stats:" + vid.String() }
func lockKey(vid uuid.UUID) string  { return "lock:stats:" + vid.String() }

// Get returns the stats for `videoID`, going through the cache → singleflight
// → distributed-lock → compute hierarchy. Returns ErrVideoNotFound if the
// video doesn't exist.
func (s *Service) Get(ctx context.Context, videoID uuid.UUID) (Stats, error) {
	if v, ok := s.readCache(ctx, videoID); ok {
		metrics.RecordStatsResult("cache_hit")
		return v, nil
	}

	// Coalesce concurrent in-process misses for the same video. singleflight
	// is intentionally per-(service, key) and uses string keys — we use the
	// raw UUID string so a re-enter for the same video finds the same slot.
	v, err, _ := s.sf.Do(videoID.String(), func() (any, error) {
		return s.fetch(ctx, videoID)
	})
	if err != nil {
		if !errors.Is(err, ErrVideoNotFound) {
			metrics.RecordStatsResult("error")
		}
		return Stats{}, err
	}
	return v.(Stats), nil
}

// fetch is what runs under the singleflight lock. It also tries the
// distributed Redis lock; whether or not that succeeds, it must return a
// Stats or an error — never block forever.
func (s *Service) fetch(ctx context.Context, videoID uuid.UUID) (Stats, error) {
	// 404 check is cheap (read-through cache on the video service). We do it
	// here, *after* the cache miss, so the fast path doesn't pay for it.
	exists, err := s.video.Exists(ctx, videoID)
	if err != nil {
		return Stats{}, fmt.Errorf("video exists check: %w", err)
	}
	if !exists {
		metrics.RecordStatsResult("not_found")
		return Stats{}, ErrVideoNotFound
	}

	// Try to acquire the distributed lock. On success we own the recompute;
	// on failure another replica is recomputing — we wait briefly, re-read
	// cache, and fall back to a private compute if the cache still hasn't
	// landed by then. That last fallback bounds tail latency: a crashed lock
	// holder cannot stall every other replica until the lock TTL elapses.
	gotLock, lockToken, err := s.acquireLock(ctx, videoID)
	if err != nil {
		metrics.RecordStatsLock("error")
		s.log.Warn("stats lock acquire failed; continuing without lock",
			slog.String("video_id", videoID.String()),
			slog.Any("error", err),
		)
		gotLock = false
	} else if gotLock {
		metrics.RecordStatsLock("acquired")
	} else {
		metrics.RecordStatsLock("contended")
	}

	if !gotLock {
		// Brief wait + cache re-read. If the holder writes the cache within
		// lockRetry we never touch the source-of-truth.
		select {
		case <-time.After(s.lockRetry):
		case <-ctx.Done():
			return Stats{}, ctx.Err()
		}
		if v, ok := s.readCache(ctx, videoID); ok {
			metrics.RecordStatsResult("cache_hit")
			return v, nil
		}
		s.log.Debug("stats lock contended, recomputing without lock",
			slog.String("video_id", videoID.String()))
	}

	computeStart := time.Now()
	stats, err := s.compute(ctx, videoID)
	metrics.RecordStatsCompute(time.Since(computeStart))
	if err != nil {
		if gotLock {
			s.releaseLock(ctx, videoID, lockToken)
		}
		return Stats{}, err
	}

	s.writeCache(ctx, stats)
	if gotLock {
		s.releaseLock(ctx, videoID, lockToken)
	}
	metrics.RecordStatsResult("computed")
	return stats, nil
}

// compute fetches views (ClickHouse) and likes (Postgres) in parallel and
// assembles the final Stats. Parallel because the two backends are
// independent — latency is max(CH, PG), not sum.
func (s *Service) compute(ctx context.Context, videoID uuid.UUID) (Stats, error) {
	var (
		viewsCount  int64
		uniqueCount int64
		likesCount  int64
	)
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		v, u, err := s.views.Counts(gctx, videoID)
		if err != nil {
			return fmt.Errorf("views: %w", err)
		}
		viewsCount, uniqueCount = v, u
		return nil
	})
	g.Go(func() error {
		l, err := s.likes.Count(gctx, videoID)
		if err != nil {
			return fmt.Errorf("likes: %w", err)
		}
		likesCount = l
		return nil
	})

	if err := g.Wait(); err != nil {
		return Stats{}, err
	}
	return Stats{
		VideoID:        videoID,
		Views:          viewsCount,
		UniqueViews:    uniqueCount,
		Likes:          likesCount,
		EngagementRate: engagementRate(likesCount, uniqueCount),
		ComputedAt:     s.clock().UTC(),
	}, nil
}

// --- Cache -------------------------------------------------------------------

func (s *Service) readCache(ctx context.Context, videoID uuid.UUID) (Stats, bool) {
	raw, err := s.rdb.Get(ctx, cacheKey(videoID)).Bytes()
	if err != nil {
		if !errors.Is(err, goredis.Nil) {
			s.log.Warn("stats cache read failed",
				slog.String("video_id", videoID.String()),
				slog.Any("error", err))
		}
		return Stats{}, false
	}
	var v Stats
	if err := json.Unmarshal(raw, &v); err != nil {
		// Corrupted cache entry — treat as miss and let the compute path
		// overwrite. Common cause is a schema change that wasn't paired with
		// a key namespace bump.
		s.log.Warn("stats cache deserialize failed",
			slog.String("video_id", videoID.String()),
			slog.Any("error", err))
		return Stats{}, false
	}
	return v, true
}

func (s *Service) writeCache(ctx context.Context, v Stats) {
	b, err := json.Marshal(v)
	if err != nil {
		s.log.Warn("stats cache marshal failed",
			slog.String("video_id", v.VideoID.String()),
			slog.Any("error", err))
		return
	}
	if err := s.rdb.Set(ctx, cacheKey(v.VideoID), b, s.cacheTTL).Err(); err != nil {
		s.log.Warn("stats cache write failed",
			slog.String("video_id", v.VideoID.String()),
			slog.Any("error", err))
	}
}

// --- Distributed lock --------------------------------------------------------

// acquireLock tries to claim the per-video recompute lock. Returns
// (acquired, token, err); the token must be passed to releaseLock so we
// don't release a lock that a successor replica has already re-acquired
// after our TTL expired (classic Redlock footgun).
func (s *Service) acquireLock(ctx context.Context, videoID uuid.UUID) (bool, string, error) {
	token := uuid.NewString()
	ok, err := s.rdb.SetNX(ctx, lockKey(videoID), token, s.lockTTL).Result()
	if err != nil {
		return false, "", err
	}
	return ok, token, nil
}

// releaseLockScript is a tiny Lua snippet that only deletes the key if its
// value equals the token we set. This is the standard "Redlock unlock" idiom
// and protects against the holder-A-stalls-then-resumes scenario.
var releaseLockScript = goredis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
end
return 0
`)

func (s *Service) releaseLock(ctx context.Context, videoID uuid.UUID, token string) {
	if token == "" {
		return
	}
	if err := releaseLockScript.Run(ctx, s.rdb, []string{lockKey(videoID)}, token).Err(); err != nil {
		// Worst case the lock expires on its own. Log and move on.
		s.log.Warn("stats lock release failed",
			slog.String("video_id", videoID.String()),
			slog.Any("error", err))
	}
}
