// Package ratelimit implements a Redis-backed leaky-bucket rate limiter.
//
// Algorithm: classical leaky bucket. A counter ("level") tracks how full a
// bucket is for a given key. Each request adds `cost` to the level; the level
// also "leaks" at a configured rate. If a request would push the level above
// capacity it is rejected and a `retry_after_ms` is returned.
//
// Why leaky bucket (vs token bucket): we want to *smooth* bursts of duplicate
// view/like events, not allow them. Token bucket permits a full-capacity burst
// the moment a bucket is refilled, which is the wrong semantics for spam
// suppression.
//
// Why Redis with a Lua script (vs an in-process counter): the API and worker
// pods are horizontally scalable; the bucket state must be shared across them
// to be effective. Lua guarantees atomicity (read-modify-write) without a
// distributed lock.
//
// Why one implementation for many use cases: by parameterising capacity and
// leak rate, the same primitive serves login rate-limiting (Step 3), view
// dedup (Step 7), and like rate-limiting (Step 6). One place to test and
// optimise.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// leakyBucketScript runs entirely server-side in Redis. The script:
//  1. Reads (level, last_ms) from a hash at KEYS[1] (defaults to (0, now)).
//  2. Leaks `leak_per_sec * elapsed` units from level (floored at 0).
//  3. Tries to add `cost`. If the result fits in capacity → allowed; else
//     compute retry-after as the time to drain enough room for `cost`.
//  4. Persists the new (level, last_ms) with a TTL so idle buckets free memory.
//  5. Returns {allowed (1/0), remaining_capacity, retry_after_ms}.
//
// Inputs:
//   KEYS[1] = bucket key
//   ARGV[1] = capacity        (int, max level)
//   ARGV[2] = leak_per_sec    (number)
//   ARGV[3] = now_ms          (int, server time in ms — Redis cannot trust local clocks)
//   ARGV[4] = cost            (number, usually 1)
//   ARGV[5] = ttl_seconds     (int, idle-key expiry)
var leakyBucketScript = redis.NewScript(`
local key       = KEYS[1]
local capacity  = tonumber(ARGV[1])
local leak_rate = tonumber(ARGV[2])
local now_ms    = tonumber(ARGV[3])
local cost      = tonumber(ARGV[4])
local ttl       = tonumber(ARGV[5])

local data    = redis.call("HMGET", key, "level", "last_ms")
local level   = tonumber(data[1]) or 0
local last_ms = tonumber(data[2]) or now_ms

local elapsed_ms = math.max(0, now_ms - last_ms)
local leaked    = (elapsed_ms / 1000) * leak_rate
level = math.max(0, level - leaked)

local new_level    = level + cost
local allowed      = 0
local retry_after  = 0

if new_level <= capacity then
    level = new_level
    allowed = 1
else
    -- not allowed; compute ms until enough has leaked to fit one 'cost' unit
    retry_after = math.ceil(((new_level - capacity) / leak_rate) * 1000)
end

redis.call("HMSET", key, "level", level, "last_ms", now_ms)
redis.call("EXPIRE", key, ttl)

local remaining = math.max(0, capacity - level)
return {allowed, math.floor(remaining), retry_after}
`)

// Policy describes the bucket parameters for a use case. Define one per
// distinct rate-limit (login, like, view, etc.) at app start and pass it in
// to Allow.
type Policy struct {
	// Capacity is the maximum level the bucket can hold before requests are rejected.
	Capacity int
	// LeakPerSecond is how many units leak out per wall-clock second.
	LeakPerSecond float64
	// TTL is the idle expiry on the bucket state. Should be comfortably larger
	// than the time it takes a full bucket to drain.
	TTL time.Duration
}

// Result describes the outcome of an Allow() call.
type Result struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// LeakyBucket is the limiter handle. Build one with New() and call Allow() on
// each request you wish to rate-limit.
type LeakyBucket struct {
	rdb *redis.Client
}

// New constructs a limiter bound to the given Redis client. The same instance
// can serve any number of bucket keys / policies concurrently.
func New(rdb *redis.Client) *LeakyBucket {
	return &LeakyBucket{rdb: rdb}
}

// Allow charges `cost` against the bucket at `key` using `policy` and reports
// whether the request is permitted. On Redis errors the caller can decide to
// fail open or fail closed; we do not assume either here.
func (lb *LeakyBucket) Allow(ctx context.Context, key string, policy Policy, cost int) (Result, error) {
	now := time.Now().UnixMilli()
	ttl := int(policy.TTL.Seconds())
	if ttl <= 0 {
		ttl = 3600
	}
	raw, err := leakyBucketScript.Run(ctx, lb.rdb, []string{key},
		policy.Capacity, policy.LeakPerSecond, now, cost, ttl,
	).Result()
	if err != nil {
		return Result{}, fmt.Errorf("leaky bucket script: %w", err)
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) != 3 {
		return Result{}, fmt.Errorf("leaky bucket: unexpected script return %v", raw)
	}
	allowed, _ := arr[0].(int64)
	remaining, _ := arr[1].(int64)
	retryAfterMs, _ := arr[2].(int64)
	return Result{
		Allowed:    allowed == 1,
		Remaining:  int(remaining),
		RetryAfter: time.Duration(retryAfterMs) * time.Millisecond,
	}, nil
}
