package view

import (
	"context"
	"log/slog"
)

// Filter is a single stage in the spam-detection pipeline. Allow returns
// (true, "") to pass an event downstream, or (false, reason) to reject it.
//
// Filters MUST be idempotent and side-effect-light: the chain may be replayed
// for retries, and operators should be able to add or remove filters without
// orchestration. Filters with mutable state (e.g. dedup writing a Redis key
// with TTL) own the contract that re-running the same input within the TTL
// window will *consistently* reject it.
//
// Name() is used for logging, metrics, and the RejectedBy field returned to
// the handler. Keep it short, lowercase_snake_case.
type Filter interface {
	Name() string
	Allow(ctx context.Context, in Input) (allowed bool, reason string)
}

// Chain is an ordered list of filters. The first filter to vote "no" short-
// circuits the rest; this is what lets us put cheap stateless filters (e.g.
// min-watch-time) before expensive ones (e.g. Redis SETNX dedup).
//
// Ordering matters: put filters in CHEAPEST → MOST EXPENSIVE order so the
// chain pays the least amount of work to reject the most common spam
// patterns. The wiring in app.go documents the canonical order.
type Chain struct {
	filters []Filter
	log     *slog.Logger
}

// NewChain builds a chain. Logging is structured: per-filter outcomes go to
// debug, rejections to info with the reason as a structured field so they
// can be aggregated by reason in log analytics.
func NewChain(log *slog.Logger, filters ...Filter) *Chain {
	return &Chain{filters: filters, log: log}
}

// Apply runs the chain. Returns (allowed, rejectedBy). On allow, rejectedBy
// is "". On reject, rejectedBy is `<filter-name>:<reason>` so dashboards can
// distinguish "rejected_by:dedup" from "rejected_by:leaky_bucket:burst".
func (c *Chain) Apply(ctx context.Context, in Input) (bool, string) {
	for _, f := range c.filters {
		ok, reason := f.Allow(ctx, in)
		if !ok {
			c.log.Debug("view filter rejected",
				slog.String("filter", f.Name()),
				slog.String("reason", reason),
				slog.String("video_id", in.VideoID.String()),
				slog.String("subject", in.SubjectKey()),
			)
			return false, f.Name() + ":" + reason
		}
	}
	return true, ""
}

// Names returns the ordered filter names. Useful for /health, metrics labels,
// and asserting wiring in tests.
func (c *Chain) Names() []string {
	out := make([]string, len(c.filters))
	for i, f := range c.filters {
		out[i] = f.Name()
	}
	return out
}
