package like

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/mhd7966/vidmerce/internal/platform/metrics"
)

// Redis key helpers. Centralised so the namespace stays consistent with
// docs/redis-keys.md.
func userLikedSetKey(uid uuid.UUID) string { return "user:" + uid.String() + ":liked:videos" }
func videoLikesCountKey(vid uuid.UUID) string { return "video:" + vid.String() + ":likes" }

// streamKey is the stream the worker consumes from.
const streamKey = "stream:likes"

// streamMaxLen is the approximate cap on the Redis stream length (XADD MAXLEN
// ~). Older entries are evicted by Redis; the worker is expected to keep up
// well before we hit this. Sized for many minutes of headroom at 1k likes/sec.
const streamMaxLen = 1_000_000

// likeScript runs the entire like / unlike hot-path inside Redis. It is the
// only place the API process mutates like state; the worker mutates Postgres
// based on the events this script emits.
//
//	KEYS[1] = user:{uid}:liked:videos  (set of video ids this user has liked)
//	KEYS[2] = video:{vid}:likes        (counter, eventual)
//	KEYS[3] = stream:likes             (event stream)
//	ARGV[1] = video_id (string)
//	ARGV[2] = user_id (string)
//	ARGV[3] = "like" | "unlike"
//	ARGV[4] = ts (ms since epoch, as string)
//	ARGV[5] = stream maxlen
//
// Returns {liked (1/0), count, status ("applied" | "noop")}.
var likeScript = goredis.NewScript(`
local userSet = KEYS[1]
local counter = KEYS[2]
local stream  = KEYS[3]
local vid     = ARGV[1]
local uid     = ARGV[2]
local op      = ARGV[3]
local ts      = ARGV[4]
local maxlen  = tonumber(ARGV[5])

local is_member = redis.call("SISMEMBER", userSet, vid)

if op == "like" then
    if is_member == 1 then
        local c = tonumber(redis.call("GET", counter)) or 0
        return {1, c, "noop"}
    end
    redis.call("SADD", userSet, vid)
    local newC = redis.call("INCR", counter)
    redis.call("XADD", stream, "MAXLEN", "~", maxlen, "*",
        "uid", uid, "vid", vid, "op", "like", "ts", ts)
    return {1, newC, "applied"}
end

if op == "unlike" then
    if is_member == 0 then
        local c = tonumber(redis.call("GET", counter)) or 0
        return {0, c, "noop"}
    end
    redis.call("SREM", userSet, vid)
    local newC = redis.call("DECR", counter)
    if newC < 0 then
        redis.call("SET", counter, "0")
        newC = 0
    end
    redis.call("XADD", stream, "MAXLEN", "~", maxlen, "*",
        "uid", uid, "vid", vid, "op", "unlike", "ts", ts)
    return {0, newC, "applied"}
end

return redis.error_reply("unknown op: " .. tostring(op))
`)

// CountRefresher is the slice of Repository the service needs to refresh the
// Redis counter from the *exact* Postgres value on cache miss. Defined here
// (consumer side) so the service is mockable in tests.
type CountRefresher interface {
	Count(ctx context.Context, videoID uuid.UUID) (int64, error)
}

// Service is the API-side like layer. It owns the Redis state and the stream
// emission; it does *not* touch Postgres directly on the hot path.
type Service struct {
	rdb  *goredis.Client
	repo CountRefresher
	log  *slog.Logger
}

// NewService wires the service. `repo` is used to repopulate the Redis
// counter from Postgres on miss; pass nil to skip that behaviour (the count
// will then be reported as 0 until a like event lands).
func NewService(rdb *goredis.Client, repo CountRefresher, log *slog.Logger) *Service {
	return &Service{rdb: rdb, repo: repo, log: log}
}

// Like applies the like operation atomically in Redis and returns the new
// state. Idempotent: liking an already-liked video is a no-op.
func (s *Service) Like(ctx context.Context, userID, videoID uuid.UUID) (State, error) {
	return s.apply(ctx, userID, videoID, OpLike)
}

// Unlike applies the unlike operation atomically. Idempotent.
func (s *Service) Unlike(ctx context.Context, userID, videoID uuid.UUID) (State, error) {
	return s.apply(ctx, userID, videoID, OpUnlike)
}

func (s *Service) apply(ctx context.Context, userID, videoID uuid.UUID, op Op) (State, error) {
	raw, err := likeScript.Run(ctx, s.rdb,
		[]string{
			userLikedSetKey(userID),
			videoLikesCountKey(videoID),
			streamKey,
		},
		videoID.String(),
		userID.String(),
		string(op),
		strconv.FormatInt(time.Now().UnixMilli(), 10),
		streamMaxLen,
	).Result()
	if err != nil {
		metrics.RecordLikeOp(string(op), "error")
		return State{}, fmt.Errorf("like script: %w", err)
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) < 2 {
		metrics.RecordLikeOp(string(op), "error")
		return State{}, fmt.Errorf("unexpected script reply: %v", raw)
	}
	liked, _ := arr[0].(int64)
	count, _ := arr[1].(int64)
	status := "applied"
	if len(arr) >= 3 {
		if s, ok := arr[2].(string); ok && s != "" {
			status = s
		}
	}
	metrics.RecordLikeOp(string(op), status)
	return State{Liked: liked == 1, Count: count}, nil
}

// IsLikedBy reports whether the user has liked the given video. Used by feed
// reads to render the "♥ filled / outline" state. Single SISMEMBER, ≈100µs.
func (s *Service) IsLikedBy(ctx context.Context, userID, videoID uuid.UUID) (bool, error) {
	got, err := s.rdb.SIsMember(ctx, userLikedSetKey(userID), videoID.String()).Result()
	if err != nil {
		return false, fmt.Errorf("sismember: %w", err)
	}
	return got, nil
}

// Count returns the current Redis counter (eventual). On cache miss it
// repopulates from Postgres if a CountRefresher was provided. The returned
// count may briefly lag the source-of-truth in Postgres by sub-second under
// normal worker load; see edge-cases.md for the bounded staleness analysis.
func (s *Service) Count(ctx context.Context, videoID uuid.UUID) (int64, error) {
	raw, err := s.rdb.Get(ctx, videoLikesCountKey(videoID)).Result()
	switch {
	case err == nil:
		n, _ := strconv.ParseInt(raw, 10, 64)
		return n, nil
	case err == goredis.Nil:
		if s.repo == nil {
			return 0, nil
		}
		n, err := s.repo.Count(ctx, videoID)
		if err != nil {
			return 0, err
		}
		// SetNX so a concurrent like event isn't clobbered by our stale read.
		_ = s.rdb.SetNX(ctx, videoLikesCountKey(videoID), n, 24*time.Hour).Err()
		return n, nil
	default:
		return 0, fmt.Errorf("counter get: %w", err)
	}
}
