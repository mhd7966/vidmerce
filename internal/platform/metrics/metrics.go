// Package metrics defines Prometheus instruments for Vidmerce and helpers
// to record business events. HTTP RED metrics are emitted by the Gin
// middleware in middleware.go; this file holds domain-specific counters and
// histograms used by services and workers.
package metrics

import (
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "vidmerce"

// Enabled gates all recording helpers. Set to false in tests to avoid
// registering duplicate collectors when multiple packages import metrics.
var Enabled = true

// --- HTTP (also populated by middleware) ---------------------------------------

var (
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "http_requests_total",
		Help:      "Total HTTP requests handled.",
	}, []string{"method", "route", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "http_request_duration_seconds",
		Help:      "HTTP request latency in seconds.",
		Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"method", "route"})

	HTTPRequestsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "http_requests_in_flight",
		Help:      "Number of HTTP requests currently being served.",
	})
)

// --- Likes ---------------------------------------------------------------------

var (
	LikeOperationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "like_operations_total",
		Help:      "Like/unlike API operations (Redis hot path).",
	}, []string{"op", "status"}) // status: applied | noop | error

	LikeWorkerApplyTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "like_worker_apply_total",
		Help:      "Like events applied to Postgres by the worker.",
	}, []string{"op", "changed"}) // changed: true | false

	LikeWorkerApplyDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "like_worker_apply_duration_seconds",
		Help:      "Duration of a single like Apply() call in the worker.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"op"})

	LikeReconcilerDriftTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "like_reconciler_drift_total",
		Help:      "Times the reconciler corrected a likes_count drift.",
	})

	LikeReconcilerCheckedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "like_reconciler_checked_total",
		Help:      "video_stats rows checked by the reconciler.",
	})
)

// --- Views ---------------------------------------------------------------------

var (
	ViewTrackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "view_track_total",
		Help:      "View track API calls.",
	}, []string{"result"}) // accepted | rejected | error

	ViewFilterRejectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "view_filter_rejections_total",
		Help:      "Views rejected by a filter in the spam chain.",
	}, []string{"filter", "reason"})

	ViewWorkerBatchesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "view_worker_batches_total",
		Help:      "ClickHouse batch flushes from the view worker.",
	})

	ViewWorkerBatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "view_worker_batch_size",
		Help:      "Number of events per ClickHouse batch flush.",
		Buckets:   []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000},
	})

	ViewWorkerInsertDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "view_worker_insert_duration_seconds",
		Help:      "ClickHouse batch insert duration.",
		Buckets:   []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5},
	})

	ViewWorkerInsertErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "view_worker_insert_errors_total",
		Help:      "Failed ClickHouse batch inserts.",
	})
)

// --- Stats ---------------------------------------------------------------------

var (
	StatsRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "stats_requests_total",
		Help:      "GET /videos/:id/stats outcomes.",
	}, []string{"result"}) // cache_hit | computed | not_found | error

	StatsComputeDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Name:      "stats_compute_duration_seconds",
		Help:      "Time to compute stats from ClickHouse + Postgres.",
		Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2},
	})

	StatsLockAcquireTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "stats_lock_acquire_total",
		Help:      "Distributed lock acquisition attempts for stats recompute.",
	}, []string{"result"}) // acquired | contended | error
)

// --- Rate limiting -------------------------------------------------------------

var (
	RateLimitHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "rate_limit_hits_total",
		Help:      "Requests rejected by leaky-bucket rate limiting.",
	}, []string{"bucket"}) // login | view | like
)

// --- Redis streams (polled by RedisCollector) ----------------------------------

var (
	RedisStreamLength = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "redis_stream_length",
		Help:      "Current length of a Redis stream (XLEN).",
	}, []string{"stream"})

	RedisStreamPending = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "redis_stream_pending",
		Help:      "Pending entries in a stream consumer group (XPENDING count).",
	}, []string{"stream", "group"})
)

// --- Recording helpers ---------------------------------------------------------

func RecordLikeOp(op string, status string) {
	if !Enabled {
		return
	}
	LikeOperationsTotal.WithLabelValues(op, status).Inc()
}

func RecordLikeWorkerApply(op string, changed bool, d time.Duration) {
	if !Enabled {
		return
	}
	ch := "false"
	if changed {
		ch = "true"
	}
	LikeWorkerApplyTotal.WithLabelValues(op, ch).Inc()
	LikeWorkerApplyDuration.WithLabelValues(op).Observe(d.Seconds())
}

func RecordViewTrack(result string) {
	if !Enabled {
		return
	}
	ViewTrackTotal.WithLabelValues(result).Inc()
}

func RecordViewFilterReject(rejectedBy string) {
	if !Enabled {
		return
	}
	filter, reason := splitRejectedBy(rejectedBy)
	ViewFilterRejectionsTotal.WithLabelValues(filter, reason).Inc()
}

func RecordViewWorkerFlush(size int, d time.Duration, err error) {
	if !Enabled {
		return
	}
	if err != nil {
		ViewWorkerInsertErrorsTotal.Inc()
		return
	}
	ViewWorkerBatchesTotal.Inc()
	ViewWorkerBatchSize.Observe(float64(size))
	ViewWorkerInsertDuration.Observe(d.Seconds())
}

func RecordStatsResult(result string) {
	if !Enabled {
		return
	}
	StatsRequestsTotal.WithLabelValues(result).Inc()
}

func RecordStatsCompute(d time.Duration) {
	if !Enabled {
		return
	}
	StatsComputeDuration.Observe(d.Seconds())
}

func RecordStatsLock(result string) {
	if !Enabled {
		return
	}
	StatsLockAcquireTotal.WithLabelValues(result).Inc()
}

func RecordRateLimit(bucket string) {
	if !Enabled {
		return
	}
	RateLimitHitsTotal.WithLabelValues(bucket).Inc()
}

func splitRejectedBy(s string) (filter, reason string) {
	if s == "" {
		return "unknown", "unknown"
	}
	if i := strings.Index(s, ":"); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, "unknown"
}
