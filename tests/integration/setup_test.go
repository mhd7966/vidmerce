//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/mhd7966/vidmerce/internal/view"
)

// pgClient returns a fresh pgxpool against the shared Postgres container.
// Callers are responsible for closing it.
func pgClient(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), s.pgDSN)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	return p
}

// redisClient returns a fresh go-redis client against the shared Redis
// container. The DB is FLUSHDB'd to give the test a clean slate.
func redisClient(t *testing.T) *goredis.Client {
	t.Helper()
	r := goredis.NewClient(&goredis.Options{Addr: s.rdAddr})
	if err := r.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("flushdb: %v", err)
	}
	return r
}

// chConn returns a fresh ClickHouse connection.
func chConn(t *testing.T) driver.Conn {
	t.Helper()
	c, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{s.chHost + ":" + s.chPort},
		Auth: clickhouse.Auth{Database: "vidmerce", Username: "default"},
	})
	if err != nil {
		t.Fatalf("clickhouse open: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("clickhouse ping: %v", err)
	}
	return c
}

// truncatePostgres clears mutable state between tests so each test starts
// from a known-empty schema. Faster than dropping/recreating tables.
func truncatePostgres(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	const q = `TRUNCATE likes, video_stats, products, videos, follows, users RESTART IDENTITY CASCADE`
	if _, err := p.Exec(context.Background(), q); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// truncateClickHouse clears the analytics tables.
func truncateClickHouse(t *testing.T, c driver.Conn) {
	t.Helper()
	for _, q := range []string{
		"TRUNCATE TABLE IF EXISTS video_views",
		"TRUNCATE TABLE IF EXISTS video_views_daily",
	} {
		if err := c.Exec(context.Background(), q); err != nil {
			t.Fatalf("truncate ch (%q): %v", q, err)
		}
	}
}

// seedVideo inserts a (user, video, video_stats) trio and returns the IDs.
// Use this for any test that needs a referenceable video to act on.
func seedVideo(t *testing.T, p *pgxpool.Pool) (videoID, ownerID uuid.UUID) {
	return seedVideoWithDuration(t, p, nil, 30)
}

// seedVideoWithDuration seeds a video with duration_sec and optionally caches
// length in Redis for the view hot path.
func seedVideoWithDuration(t *testing.T, p *pgxpool.Pool, rdb *goredis.Client, durationSec int) (videoID, ownerID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	ownerID = uuid.New()
	if _, err := p.Exec(ctx, `
		INSERT INTO users (id, email, password_hash)
		VALUES ($1, $2, 'x')
	`, ownerID, "test+"+ownerID.String()+"@vidmerce.test"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	videoID = uuid.New()
	if _, err := p.Exec(ctx, `
		INSERT INTO videos (id, user_id, title, description, video_url, duration_sec)
		VALUES ($1, $2, 'integration', 'integration', 'https://example.com/v.mp4', $3)
	`, videoID, ownerID, durationSec); err != nil {
		t.Fatalf("seed video: %v", err)
	}
	if rdb != nil {
		if err := view.NewRedisDurationStore(rdb, time.Hour).Set(ctx, videoID, durationSec); err != nil {
			t.Fatalf("seed duration cache: %v", err)
		}
	}
	// The video_stats row should have been created by the AFTER INSERT
	// trigger in 0001_init. Verify so a future schema change can't silently
	// regress the assumption.
	var got int64
	if err := p.QueryRow(ctx,
		`SELECT likes_count FROM video_stats WHERE video_id = $1`, videoID,
	).Scan(&got); err != nil {
		t.Fatalf("video_stats row missing: %v", err)
	}
	return videoID, ownerID
}

// waitFor polls fn() until it returns true or the deadline elapses. Useful
// for asynchronous side effects (worker has processed a stream entry, MV has
// caught up, etc.) where polling is the only signal available.
func waitFor(t *testing.T, timeout time.Duration, label string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitFor %q timed out after %s", label, timeout)
}

// runUntilDrained starts the worker's Run loop in a goroutine and waits
// until the consumer group has no pending messages on `stream`, then cancels
// the context to stop the worker.
func runUntilDrained(t *testing.T, run func(context.Context) error, rdb *goredis.Client, stream, group string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	waitFor(t, 5*time.Second, "stream drained", func() bool {
		pending, err := rdb.XPending(context.Background(), stream, group).Result()
		if err != nil {
			return false
		}
		return pending.Count == 0
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop after cancel")
	}
}
