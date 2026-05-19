package metrics

import (
	"context"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// RunRedisCollector polls Redis stream lengths and consumer-group pending
// counts on a fixed interval until ctx is cancelled. Intended to run in both
// API and worker processes (duplicate scrapes are harmless — same values).
func RunRedisCollector(ctx context.Context, rdb *goredis.Client, consumerGroup string, interval time.Duration, log *slog.Logger) {
	if !Enabled || rdb == nil {
		return
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	poll := func() {
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for _, stream := range []string{"stream:likes", "stream:views"} {
			if n, err := rdb.XLen(pollCtx, stream).Result(); err == nil {
				RedisStreamLength.WithLabelValues(stream).Set(float64(n))
			} else if log != nil {
				log.Debug("metrics: xlen failed", slog.String("stream", stream), slog.Any("error", err))
			}
			if consumerGroup != "" {
				if pending, err := rdb.XPending(pollCtx, stream, consumerGroup).Result(); err == nil {
					RedisStreamPending.WithLabelValues(stream, consumerGroup).Set(float64(pending.Count))
				}
			}
		}
	}

	poll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			poll()
		}
	}
}
