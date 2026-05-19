package metrics

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// Prometheus returns Gin middleware that records request count, latency, and
// in-flight gauge. Uses c.FullPath() for the route label so cardinality stays
// bounded to registered routes (not raw URLs with UUIDs).
func Prometheus() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !Enabled {
			c.Next()
			return
		}
		route := c.FullPath()
		if route == "" {
			route = "unknown"
		}
		method := c.Request.Method

		HTTPRequestsInFlight.Inc()
		start := time.Now()
		c.Next()
		HTTPRequestsInFlight.Dec()

		status := strconv.Itoa(c.Writer.Status())
		HTTPRequestsTotal.WithLabelValues(method, route, status).Inc()
		HTTPRequestDuration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
	}
}
