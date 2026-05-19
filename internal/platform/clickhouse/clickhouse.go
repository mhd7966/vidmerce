// Package clickhouse wraps the ClickHouse connection. Centralising
// construction here means TLS, compression, and DDL bootstrapping live in
// exactly one place and the rest of the codebase only depends on the Conn
// type returned by NewConn.
package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/mhd7966/vidmerce/internal/platform/config"
)

// Conn is the application's ClickHouse handle. Type-aliased so callers can
// depend on driver.Conn (an interface) rather than the concrete client.
type Conn = driver.Conn

// NewConn builds a configured ClickHouse connection and verifies it with a
// Ping. Returns an error if either step fails.
func NewConn(ctx context.Context, cfg config.ClickHouseConfig) (Conn, error) {
	opts := &clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.User,
			Password: cfg.Password,
		},
		DialTimeout: 5 * time.Second,
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
	}

	c, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	return c, nil
}
