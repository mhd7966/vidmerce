// Package config loads typed application configuration from environment
// variables (and optionally a .env file) via struct tags. Centralising config
// here lets the rest of the codebase depend on a struct instead of os.Getenv
// calls scattered everywhere, which makes testing and validation trivial.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ilyakaznacheev/cleanenv"
)

// FeedMode controls which feed implementation the API serves.
type FeedMode string

const (
	FeedModePull FeedMode = "pull"
	FeedModePush FeedMode = "push"
)

// Config is the fully-resolved application configuration. Field tags declare
// the env-var name, default value, and (when applicable) whether the value is
// required. Validation invariants live in (Config).validate() below.
//
// Each nested struct represents one concern (HTTP, Postgres, Redis, ...) so
// that subsystems can be passed a small typed slice of the config and not the
// whole world.
type Config struct {
	AppEnv string `env:"APP_ENV" env-default:"local"`

	HTTP       HTTPConfig
	Log        LogConfig
	Postgres   PostgresConfig
	Redis      RedisConfig
	ClickHouse ClickHouseConfig
	JWT        JWTConfig
	Feed       FeedConfig
	View       ViewConfig
	Like       LikeConfig
	Stats      StatsConfig
	Metrics    MetricsConfig
	Worker     WorkerConfig
	Bloom      BloomConfig

	BcryptCost int `env:"BCRYPT_COST" env-default:"12"`
}

type HTTPConfig struct {
	Port            int           `env:"HTTP_PORT" env-default:"8080"`
	ReadTimeout     time.Duration `env:"HTTP_READ_TIMEOUT" env-default:"10s"`
	WriteTimeout    time.Duration `env:"HTTP_WRITE_TIMEOUT" env-default:"15s"`
	ShutdownTimeout time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT" env-default:"15s"`
}

type LogConfig struct {
	Level  string `env:"LOG_LEVEL" env-default:"info"`  // debug | info | warn | error
	Format string `env:"LOG_FORMAT" env-default:"text"` // text | json
}

type PostgresConfig struct {
	DSN             string        `env:"POSTGRES_DSN" env-default:"postgres://vidmerce:vidmerce@localhost:5432/vidmerce?sslmode=disable"`
	MaxOpenConns    int           `env:"POSTGRES_MAX_OPEN_CONNS" env-default:"20"`
	MaxIdleConns    int           `env:"POSTGRES_MAX_IDLE_CONNS" env-default:"10"`
	ConnMaxLifetime time.Duration `env:"POSTGRES_CONN_LIFETIME" env-default:"30m"`
}

type RedisConfig struct {
	Addr     string `env:"REDIS_ADDR" env-default:"localhost:6379"`
	DB       int    `env:"REDIS_DB" env-default:"0"`
	Password string `env:"REDIS_PASSWORD" env-default:""`
}

type ClickHouseConfig struct {
	Addr     string `env:"CLICKHOUSE_ADDR" env-default:"localhost:9000"`
	Database string `env:"CLICKHOUSE_DB" env-default:"vidmerce"`
	User     string `env:"CLICKHOUSE_USER" env-default:"default"`
	Password string `env:"CLICKHOUSE_PASSWORD" env-default:""`
}

type JWTConfig struct {
	Secret     string        `env:"JWT_SECRET" env-default:"dev-only-secret-change-me"`
	AccessTTL  time.Duration `env:"JWT_ACCESS_TTL" env-default:"15m"`
	RefreshTTL time.Duration `env:"JWT_REFRESH_TTL" env-default:"168h"`
}

type FeedConfig struct {
	// pull = Postgres keyset pagination | push = Redis ZSET fan-out cache
	Mode        FeedMode `env:"FEED_MODE" env-default:"pull"`
	PageDefault int      `env:"FEED_PAGE_DEFAULT" env-default:"20"`
	PageMax     int      `env:"FEED_PAGE_MAX" env-default:"50"`
	PushZSetCap int      `env:"FEED_PUSH_ZSET_CAP" env-default:"1000"`
}

type ViewConfig struct {
	// UniqueTTL: first view per (subject, video) in this window sets is_unique=1.
	UniqueTTL time.Duration `env:"VIEW_UNIQUE_TTL" env-default:"10m"`
	// MinDurationSec: shorter videos skip the duration-based rate bucket only.
	MinDurationSec int `env:"VIEW_MIN_DURATION_SEC" env-default:"5"`
	// UnknownMinWatchMs: min watch when duration is missing from Redis cache.
	UnknownMinWatchMs int `env:"VIEW_UNKNOWN_MIN_WATCH_MS" env-default:"1000"`
	// DurationCacheTTL: Redis TTL for video:{id}:dur (written at create).
	DurationCacheTTL time.Duration `env:"VIEW_DURATION_CACHE_TTL" env-default:"168h"`
}

type LikeConfig struct {
	BucketCapacity       int           `env:"LIKE_LEAKY_BUCKET_CAPACITY" env-default:"10"`
	BucketLeakPerMin     int           `env:"LIKE_LEAKY_BUCKET_LEAK_PER_MIN" env-default:"5"`
	ReconcilerInterval   time.Duration `env:"LIKE_RECONCILER_INTERVAL" env-default:"1h"`
	ReconcilerSampleSize int           `env:"LIKE_RECONCILER_SAMPLE_SIZE" env-default:"200"`
}

type StatsConfig struct {
	// CacheTTL bounds how stale a /stats response can be. Short enough for
	// content creators to see "near real-time" feedback, long enough to keep
	// the ClickHouse read pressure flat.
	CacheTTL time.Duration `env:"STATS_CACHE_TTL" env-default:"30s"`
	// LockTTL bounds how long a distributed recompute lock is held. Sized to
	// be comfortably larger than a worst-case CH query so the lock never
	// expires while a compute is still running.
	LockTTL time.Duration `env:"STATS_LOCK_TTL" env-default:"5s"`
	// LockRetry is how long a contender for the lock will wait (re-reading
	// cache) before giving up and computing without the lock. Keeps p99 low
	// even when a holder dies before unlocking.
	LockRetry time.Duration `env:"STATS_LOCK_RETRY" env-default:"75ms"`
}

type MetricsConfig struct {
	Enabled           bool          `env:"METRICS_ENABLED" env-default:"true"`
	Path              string        `env:"METRICS_PATH" env-default:"/metrics"`
	WorkerPort        int           `env:"METRICS_WORKER_PORT" env-default:"9091"`
	RedisPollInterval time.Duration `env:"METRICS_REDIS_POLL_INTERVAL" env-default:"15s"`
}

type WorkerConfig struct {
	ConsumerGroup    string        `env:"WORKER_CONSUMER_GROUP" env-default:"vidmerce-workers"`
	Name             string        `env:"WORKER_NAME" env-default:"worker-1"`
	StreamPartitions int           `env:"WORKER_STREAM_PARTITIONS" env-default:"8"`
	BatchSize        int           `env:"WORKER_BATCH_SIZE" env-default:"500"`
	BatchTimeout    time.Duration `env:"WORKER_BATCH_TIMEOUT" env-default:"1s"`
}

// BloomConfig controls RedisBloom filters used as a fast front for Postgres
// UNIQUE constraints (emails, one-product-per-video). Requires Redis Stack.
type BloomConfig struct {
	Enabled        bool    `env:"BLOOM_ENABLED" env-default:"true"`
	ErrorRate      float64 `env:"BLOOM_ERROR_RATE" env-default:"0.01"`
	EmailCapacity  int64   `env:"BLOOM_EMAIL_CAPACITY" env-default:"1000000"`
	ProductCapacity int64  `env:"BLOOM_PRODUCT_CAPACITY" env-default:"500000"`
}

// Load reads configuration from the process environment, optionally merging
// in values from a .env-style file at `path` when it exists. Returns an
// error only for values that *must* be present (e.g. JWT secret in prod) or
// for invariant violations.
func Load(path string) (Config, error) {
	var c Config

	// If a .env file is present we read it first so that anything set in the
	// process env still wins (cleanenv.ReadConfig honours that precedence).
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if err := cleanenv.ReadConfig(path, &c); err != nil {
				return c, fmt.Errorf("read env file %q: %w", path, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return c, fmt.Errorf("stat env file %q: %w", path, err)
		}
	}

	// Always re-read env vars after the file so the live environment is the
	// final authority. This makes the call idempotent and predictable.
	if err := cleanenv.ReadEnv(&c); err != nil {
		return c, fmt.Errorf("read env: %w", err)
	}

	// Normalise enum-like fields before validation so case-insensitive values
	// ("PULL", "Pull", "pull") all resolve consistently.
	c.Feed.Mode = FeedMode(strings.ToLower(string(c.Feed.Mode)))

	return c, c.validate()
}

func (c Config) validate() error {
	if c.Feed.Mode != FeedModePull && c.Feed.Mode != FeedModePush {
		return fmt.Errorf("FEED_MODE must be 'pull' or 'push', got %q", c.Feed.Mode)
	}
	if c.AppEnv == "production" && c.JWT.Secret == "dev-only-secret-change-me" {
		return fmt.Errorf("JWT_SECRET must be set in production")
	}
	if c.HTTP.Port <= 0 || c.HTTP.Port > 65535 {
		return fmt.Errorf("HTTP_PORT out of range: %d", c.HTTP.Port)
	}
	if c.Postgres.MaxIdleConns > c.Postgres.MaxOpenConns {
		return fmt.Errorf("POSTGRES_MAX_IDLE_CONNS (%d) must be <= POSTGRES_MAX_OPEN_CONNS (%d)",
			c.Postgres.MaxIdleConns, c.Postgres.MaxOpenConns)
	}
	return nil
}
