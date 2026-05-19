package view

import (
	"time"

	"github.com/mhd7966/vidmerce/internal/platform/ratelimit"
)

// ViewPolicyConfig holds tunables for duration-aware view rules.
type ViewPolicyConfig struct {
	// MinDurationSec: videos shorter than this skip the duration-based rate
	// bucket (still subject to watch threshold rules below).
	MinDurationSec int
	// UnknownMinWatchMs is used when duration is not in Redis (cache miss).
	UnknownMinWatchMs int
}

// RequiredWatchMs returns the minimum client-reported watch_ms for a counted
// view. Rule: watch at least one third of the video length. Videos under
// MinDurationSec use UnknownMinWatchMs only (no 1/3 rule).
func RequiredWatchMs(durationSec int, cfg ViewPolicyConfig) int {
	minDur := cfg.MinDurationSec
	if minDur <= 0 {
		minDur = 5
	}
	unknownMin := cfg.UnknownMinWatchMs
	if durationSec <= 0 {
		return unknownMin
	}
	if durationSec < minDur {
		return unknownMin
	}
	w := (durationSec * 1000) / 3
	if w < 1 {
		w = 1
	}
	return w
}

// RatePolicy returns the leaky-bucket policy for a video length and whether
// the bucket applies. Physics cap: at most 60/duration full watches per minute.
// Videos under MinDurationSec skip the bucket (fail-open at filter).
func RatePolicy(durationSec int, cfg ViewPolicyConfig) (ratelimit.Policy, bool) {
	if cfg.MinDurationSec <= 0 {
		cfg.MinDurationSec = 5
	}
	if durationSec < cfg.MinDurationSec {
		return ratelimit.Policy{}, false
	}
	perMin := 60 / durationSec
	if perMin < 1 {
		perMin = 1
	}
	return ratelimit.Policy{
		Capacity:      perMin,
		LeakPerSecond: float64(perMin) / 60.0,
		TTL:           time.Hour,
	}, true
}
