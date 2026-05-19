// Package httpx holds shared HTTP plumbing: middleware, error helpers, and
// response shapes. Keeping these out of the feature packages prevents
// duplication and gives us a single place to evolve request/response
// conventions (request IDs, error envelopes, etc.).
package httpx

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// HeaderRequestID is the canonical request-correlation header. We accept it
// from upstream proxies if present, otherwise we mint one. Returning the same
// value in the response makes log-grep across services possible.
const HeaderRequestID = "X-Request-ID"

// CtxKeyRequestID is the Gin context key used to stash the request ID.
const CtxKeyRequestID = "request_id"

// RequestID sets a request ID on every request, propagating any value sent by
// an upstream caller (e.g. an LB or API gateway) and minting a UUIDv4 otherwise.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(HeaderRequestID)
		if rid == "" {
			rid = uuid.NewString()
		}
		c.Set(CtxKeyRequestID, rid)
		c.Writer.Header().Set(HeaderRequestID, rid)
		c.Next()
	}
}

// AccessLog emits one structured log line per request, including status,
// latency and the request ID. We log at Info for 2xx/3xx, Warn for 4xx, and
// Error for 5xx so on-call can grep severity directly.
func AccessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		attrs := []any{
			slog.String("method", c.Request.Method),
			slog.String("path", c.FullPath()),
			slog.Int("status", status),
			slog.Duration("latency", time.Since(start)),
			slog.String("client_ip", c.ClientIP()),
			slog.String("request_id", c.GetString(CtxKeyRequestID)),
		}
		switch {
		case status >= 500:
			log.Error("request", attrs...)
		case status >= 400:
			log.Warn("request", attrs...)
		default:
			log.Info("request", attrs...)
		}
	}
}

// Recover converts panics into a 500 response and logs the stack at error
// level. We use gin.CustomRecovery under the hood so the response matches our
// error envelope.
func Recover(log *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		log.Error("panic recovered",
			slog.Any("error", recovered),
			slog.String("path", c.FullPath()),
			slog.String("request_id", c.GetString(CtxKeyRequestID)),
		)
		Error(c, 500, CodeInternal, "internal server error")
	})
}
