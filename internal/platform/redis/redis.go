// Package redis wraps the Redis client. As with the Postgres wrapper, the
// rest of the codebase depends on the type defined here so connection options
// and tracing can be added in exactly one place.
package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/mhd7966/vidmerce/internal/platform/config"
)

// Client is the application's Redis handle. Type-aliased so consumers can
// mock the interface they need (Pinger, KV operations, Streams) without
// importing go-redis directly.
type Client = goredis.Client

// NewClient builds a configured Redis client and verifies it with a PING.
// Returns an error if either step fails.
func NewClient(ctx context.Context, cfg config.RedisConfig) (*Client, error) {
	c := goredis.NewClient(&goredis.Options{
		Addr:     cfg.Addr,
		DB:       cfg.DB,
		Password: cfg.Password,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return c, nil
}
