package ratelimit

import (
	"log/slog"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mhd7966/vidmerce/internal/platform/httpx"
	"github.com/mhd7966/vidmerce/internal/platform/metrics"
)

// KeyFunc derives a bucket key from the request. Examples:
//   - ByIP: "login:" + c.ClientIP()
//   - ByUser: "like:" + userID.String() + ":" + videoID.String()
type KeyFunc func(c *gin.Context) string

// MiddlewareConfig parameterises the Gin middleware factory below.
type MiddlewareConfig struct {
	Bucket  *LeakyBucket
	Policy  Policy
	KeyFunc KeyFunc
	// Logger is used for "fail-open" decisions on Redis errors. Required so
	// silent failures are visible in operations.
	Logger *slog.Logger
	// FailOpen=true lets requests through if Redis is unreachable. Usually we
	// want this for non-security-critical limits (e.g. view dedup); for
	// security-critical limits (login) set this to false.
	FailOpen bool
	// MetricLabel is the `bucket` label on vidmerce_rate_limit_hits_total
	// (e.g. login, like, view).
	MetricLabel string
}

// Middleware returns a Gin middleware that applies the given leaky-bucket
// policy. On rate-limit hits, it responds with 429 and a Retry-After header.
func Middleware(cfg MiddlewareConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := cfg.KeyFunc(c)
		res, err := cfg.Bucket.Allow(c.Request.Context(), key, cfg.Policy, 1)
		if err != nil {
			cfg.Logger.Error("rate-limit backend error",
				slog.String("key", key),
				slog.Any("error", err),
			)
			if cfg.FailOpen {
				c.Next()
				return
			}
			httpx.Error(c, 503, httpx.CodeServiceUnready, "rate limiter unavailable")
			return
		}
		if !res.Allowed {
			if cfg.MetricLabel != "" {
				metrics.RecordRateLimit(cfg.MetricLabel)
			}
			retryAfter := int(res.RetryAfter / time.Second)
			if retryAfter < 1 {
				retryAfter = 1
			}
			c.Header("Retry-After", strconv.Itoa(retryAfter))
			httpx.ErrorDetail(c, 429, httpx.CodeRateLimited, "too many requests", gin.H{
				"retry_after_seconds": retryAfter,
			})
			return
		}
		c.Next()
	}
}
