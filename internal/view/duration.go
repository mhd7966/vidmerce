package view

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// DurationKey is the Redis string key holding video length in seconds. Written
// at video create time; the view hot path only does GET (no Postgres).
func DurationKey(videoID uuid.UUID) string {
	return "video:" + videoID.String() + ":dur"
}

// DurationStore reads/writes cached video lengths for the view pipeline.
type DurationStore interface {
	Get(ctx context.Context, videoID uuid.UUID) (sec int, ok bool)
	Set(ctx context.Context, videoID uuid.UUID, sec int) error
}

// RedisDurationStore implements DurationStore.
type RedisDurationStore struct {
	rdb *goredis.Client
	ttl time.Duration
}

// NewRedisDurationStore caches durations. ttl=0 keeps keys without expiry.
func NewRedisDurationStore(rdb *goredis.Client, ttl time.Duration) *RedisDurationStore {
	return &RedisDurationStore{rdb: rdb, ttl: ttl}
}

func (s *RedisDurationStore) Get(ctx context.Context, videoID uuid.UUID) (int, bool) {
	raw, err := s.rdb.Get(ctx, DurationKey(videoID)).Result()
	if err == goredis.Nil {
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	sec, err := strconv.Atoi(raw)
	if err != nil || sec <= 0 {
		return 0, false
	}
	return sec, true
}

func (s *RedisDurationStore) Set(ctx context.Context, videoID uuid.UUID, sec int) error {
	if sec <= 0 {
		return fmt.Errorf("duration_sec must be positive")
	}
	if s.ttl > 0 {
		return s.rdb.Set(ctx, DurationKey(videoID), strconv.Itoa(sec), s.ttl).Err()
	}
	return s.rdb.Set(ctx, DurationKey(videoID), strconv.Itoa(sec), 0).Err()
}
