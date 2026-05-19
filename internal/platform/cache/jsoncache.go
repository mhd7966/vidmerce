// Package cache provides a tiny, generic JSON-over-Redis cache that the
// feature services (video, product, stats) compose into their read paths.
//
// The point of having one helper instead of inlining the same Get/Set/Del
// dance in every service is consistency: a single place to add tracing,
// stampede protection, or a serialisation change later. The type parameter
// keeps callers honest — you can't accidentally Set a Video into a Product
// cache.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// JSONCache stores values of type T as JSON in Redis under keys produced by
// KeyFn. Build one per (type, namespace) pair at app start.
type JSONCache[T any] struct {
	rdb   *goredis.Client
	keyFn func(id string) string
	ttl   time.Duration
}

// New constructs a cache. `keyFn` turns a logical id into the full Redis key
// (e.g. id "abc" -> "video:abc"). ttl <= 0 means "no expiry" — use sparingly.
func New[T any](rdb *goredis.Client, keyFn func(id string) string, ttl time.Duration) *JSONCache[T] {
	return &JSONCache[T]{rdb: rdb, keyFn: keyFn, ttl: ttl}
}

// Get returns the cached value and a hit flag. A cache miss is not an error;
// only an unexpected Redis or deserialisation failure produces an error.
func (c *JSONCache[T]) Get(ctx context.Context, id string) (T, bool, error) {
	var zero T
	raw, err := c.rdb.Get(ctx, c.keyFn(id)).Bytes()
	if errors.Is(err, goredis.Nil) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("cache get: %w", err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		// A bad cache entry (e.g. after a schema change) is recoverable: we
		// surface it as a miss + best-effort delete so the next read repopulates.
		_ = c.rdb.Del(ctx, c.keyFn(id)).Err()
		return zero, false, nil
	}
	return v, true, nil
}

// Set persists v under id with the configured TTL.
func (c *JSONCache[T]) Set(ctx context.Context, id string, v T) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cache marshal: %w", err)
	}
	if err := c.rdb.Set(ctx, c.keyFn(id), b, c.ttl).Err(); err != nil {
		return fmt.Errorf("cache set: %w", err)
	}
	return nil
}

// Del removes the entry under id. Idempotent.
func (c *JSONCache[T]) Del(ctx context.Context, id string) error {
	if err := c.rdb.Del(ctx, c.keyFn(id)).Err(); err != nil {
		return fmt.Errorf("cache del: %w", err)
	}
	return nil
}
