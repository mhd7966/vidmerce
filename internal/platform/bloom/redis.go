package bloom

import (
	"context"
	"fmt"
	"strings"

	goredis "github.com/redis/go-redis/v9"
)

// RedisFilter wraps RedisBloom commands (BF.*) for one logical filter.
type RedisFilter struct {
	rdb       *goredis.Client
	key       string
	errorRate float64
	capacity  int64
}

// NewRedisFilter builds a filter. errorRate is the false-positive rate passed
// to BF.RESERVE (e.g. 0.01 = 1%). capacity is the expected number of members.
func NewRedisFilter(rdb *goredis.Client, key string, errorRate float64, capacity int64) *RedisFilter {
	if errorRate <= 0 {
		errorRate = 0.01
	}
	if capacity <= 0 {
		capacity = 1_000_000
	}
	return &RedisFilter{rdb: rdb, key: key, errorRate: errorRate, capacity: capacity}
}

// Init creates the Bloom filter key if it does not exist (BF.RESERVE).
func (f *RedisFilter) Init(ctx context.Context) error {
	_, err := f.rdb.Do(ctx, "BF.RESERVE", f.key, f.errorRate, f.capacity).Result()
	if err == nil {
		return nil
	}
	// Key already reserved — not an error.
	if strings.Contains(strings.ToLower(err.Error()), "exists") {
		return nil
	}
	// BF.RESERVE returns an error if the key exists with another type; INFO works.
	if _, infoErr := f.rdb.Do(ctx, "BF.INFO", f.key).Result(); infoErr == nil {
		return nil
	}
	return fmt.Errorf("bf.reserve %s: %w", f.key, err)
}

// MayContain implements Filter using BF.EXISTS.
func (f *RedisFilter) MayContain(ctx context.Context, member string) (bool, error) {
	res, err := f.rdb.Do(ctx, "BF.EXISTS", f.key, member).Result()
	if err != nil {
		return false, fmt.Errorf("bf.exists: %w", err)
	}
	switch v := res.(type) {
	case int64:
		return v == 1, nil
	case bool:
		return v, nil
	default:
		return false, fmt.Errorf("bf.exists: unexpected type %T", res)
	}
}

// Add implements Filter using BF.ADD.
func (f *RedisFilter) Add(ctx context.Context, member string) error {
	if _, err := f.rdb.Do(ctx, "BF.ADD", f.key, member).Result(); err != nil {
		return fmt.Errorf("bf.add: %w", err)
	}
	return nil
}

// AddBatch bulk-loads members with BF.MADD (used during warmup).
func (f *RedisFilter) AddBatch(ctx context.Context, members []string) error {
	if len(members) == 0 {
		return nil
	}
	cmd := make([]interface{}, 0, 2+len(members))
	cmd = append(cmd, "BF.MADD", f.key)
	for _, m := range members {
		cmd = append(cmd, m)
	}
	if _, err := f.rdb.Do(ctx, cmd...).Result(); err != nil {
		return fmt.Errorf("bf.madd: %w", err)
	}
	return nil
}
