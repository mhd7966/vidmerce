// Package health serves liveness (/health) and readiness (/ready) probes.
// It is the first feature package in the codebase to demonstrate the
// constructor-injection convention: the handler is built with everything it
// needs (config + a set of dependency Pingers) and exposes plain methods that
// satisfy gin.HandlerFunc.
//
// Liveness vs readiness:
//
//	/health  — process is alive and the router is responsive. Used by load
//	           balancers to decide whether to keep this instance in rotation.
//	/ready   — all downstream dependencies (Postgres, Redis, ClickHouse) are
//	           reachable. Used by orchestrators to decide whether to start
//	           sending traffic.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mhd7966/vidmerce/internal/platform/config"
	"github.com/mhd7966/vidmerce/internal/platform/httpx"
)

// Pinger is the minimum interface a dependency must satisfy to participate in
// the readiness probe. Defining it here (consumer side) keeps the handler
// decoupled from any concrete datastore client.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PingerFunc adapts a plain function to the Pinger interface. Useful when
// wiring up clients whose own Ping signatures differ slightly from ours.
type PingerFunc func(ctx context.Context) error

// Ping implements Pinger.
func (f PingerFunc) Ping(ctx context.Context) error { return f(ctx) }

// Handler renders the two probes. Build it once at app start via NewHandler.
type Handler struct {
	cfg  config.Config
	deps map[string]Pinger
}

// NewHandler wires the probes. The `deps` map is the set of dependencies
// whose health is reported by /ready; the keys are stable identifiers shown
// in the response (e.g. "postgres", "redis", "clickhouse"). Pass an empty
// map for a degenerate readiness probe that always succeeds.
func NewHandler(cfg config.Config, deps map[string]Pinger) *Handler {
	if deps == nil {
		deps = map[string]Pinger{}
	}
	return &Handler{cfg: cfg, deps: deps}
}

// Health is a liveness probe. It does not touch any dependency.
func (h *Handler) Health(c *gin.Context) {
	httpx.OK(c, gin.H{
		"app_env":   h.cfg.AppEnv,
		"feed_mode": h.cfg.Feed.Mode,
		"time":      time.Now().UTC().Format(time.RFC3339),
	})
}

// Ready pings every configured dependency and returns 503 if any of them
// fails. The response body always carries per-dependency status so on-call
// can see at a glance which component is unhealthy.
func (h *Handler) Ready(c *gin.Context) {
	const probeTimeout = 2 * time.Second
	ctx, cancel := context.WithTimeout(c.Request.Context(), probeTimeout)
	defer cancel()

	results := make(map[string]string, len(h.deps))
	allOK := true
	for name, p := range h.deps {
		if err := p.Ping(ctx); err != nil {
			results[name] = "down: " + err.Error()
			allOK = false
			continue
		}
		results[name] = "ok"
	}

	if !allOK {
		httpx.ErrorDetail(c, http.StatusServiceUnavailable,
			httpx.CodeServiceUnready, "one or more dependencies are unavailable", results)
		return
	}
	httpx.OK(c, results)
}
