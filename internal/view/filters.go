package view

import (
	"context"
	"log/slog"

	"github.com/mhd7966/vidmerce/internal/platform/ratelimit"
)

// --- WatchThresholdFilter ----------------------------------------------------

// WatchThresholdFilter rejects views whose client-reported watch_ms is below
// RequiredWatchMs(duration). DurationSec on Input must be set by the service
// (0 = unknown → UnknownMinWatchMs from policy config).
type WatchThresholdFilter struct {
	Policy ViewPolicyConfig
}

func NewWatchThresholdFilter(cfg ViewPolicyConfig) *WatchThresholdFilter {
	return &WatchThresholdFilter{Policy: cfg}
}

func (*WatchThresholdFilter) Name() string { return "watch_threshold" }

func (f *WatchThresholdFilter) Allow(_ context.Context, in Input) (bool, string) {
	need := RequiredWatchMs(in.DurationSec, f.Policy)
	if need <= 0 {
		return true, ""
	}
	if in.WatchMs < need {
		return false, "below_threshold"
	}
	return true, ""
}

// MinWatchTimeFilter is deprecated; use WatchThresholdFilter.
func NewMinWatchTimeFilter(minMs int) *WatchThresholdFilter {
	if minMs <= 0 {
		return NewWatchThresholdFilter(ViewPolicyConfig{UnknownMinWatchMs: 0, MinDurationSec: 1})
	}
	return NewWatchThresholdFilter(ViewPolicyConfig{UnknownMinWatchMs: minMs, MinDurationSec: 1})
}

// --- DurationRateFilter ------------------------------------------------------

// DurationRateFilter applies a per-(subject, video) leaky bucket sized from
// video length (max ~60/duration views per minute). Skipped when duration is
// unknown or shorter than MinDurationSec.
type DurationRateFilter struct {
	bucket   *ratelimit.LeakyBucket
	policy   ViewPolicyConfig
	log      *slog.Logger
	failOpen bool
}

func NewDurationRateFilter(bucket *ratelimit.LeakyBucket, policy ViewPolicyConfig, log *slog.Logger, failOpen bool) *DurationRateFilter {
	return &DurationRateFilter{bucket: bucket, policy: policy, log: log, failOpen: failOpen}
}

func (*DurationRateFilter) Name() string { return "duration_rate" }

func (f *DurationRateFilter) Allow(ctx context.Context, in Input) (bool, string) {
	rl, ok := RatePolicy(in.DurationSec, f.policy)
	if !ok {
		return true, ""
	}
	key := "bucket:view:" + in.SubjectKey() + ":" + in.VideoID.String()
	r, err := f.bucket.Allow(ctx, key, rl, 1)
	if err != nil {
		f.log.Warn("duration rate filter error",
			slog.String("key", key), slog.Any("error", err))
		if f.failOpen {
			return true, ""
		}
		return false, "infra_error"
	}
	if !r.Allowed {
		return false, "rate_limited"
	}
	return true, ""
}

// LeakyBucketFilter is deprecated; use DurationRateFilter.
func NewLeakyBucketFilter(bucket *ratelimit.LeakyBucket, policy ratelimit.Policy, log *slog.Logger, failOpen bool) *DurationRateFilter {
	_ = policy
	return NewDurationRateFilter(bucket, ViewPolicyConfig{MinDurationSec: 5}, log, failOpen)
}
