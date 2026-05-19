// Package db wraps the Postgres connection pool. The rest of the codebase
// depends on the *pgxpool.Pool returned here rather than constructing one
// directly, so retry policy, telemetry, and pool sizing live in exactly one
// place.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mhd7966/vidmerce/internal/platform/config"
)

// Pool is the application's Postgres handle. We type-alias *pgxpool.Pool so
// callers can mock against this package's interface (Pinger) in tests instead
// of importing pgx directly.
type Pool = pgxpool.Pool

// NewPool builds a configured connection pool and verifies it with a Ping.
// Returns an error if either step fails so the caller never gets a half-broken
// pool back.
func NewPool(ctx context.Context, cfg config.PostgresConfig) (*Pool, error) {
	pcfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	pcfg.MaxConns = int32(cfg.MaxOpenConns)
	pcfg.MinConns = int32(cfg.MaxIdleConns)
	pcfg.MaxConnLifetime = cfg.ConnMaxLifetime

	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}
