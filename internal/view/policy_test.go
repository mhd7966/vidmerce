package view

import (
	"testing"

	"github.com/mhd7966/vidmerce/internal/platform/ratelimit"
)

func TestRequiredWatchMs(t *testing.T) {
	cfg := ViewPolicyConfig{MinDurationSec: 5, UnknownMinWatchMs: 1000}
	if got := RequiredWatchMs(30, cfg); got != 10000 {
		t.Fatalf("30s video want 10000ms (1/3), got %d", got)
	}
	if got := RequiredWatchMs(4, cfg); got != 1000 {
		t.Fatalf("short video should use unknown min, got %d", got)
	}
	if got := RequiredWatchMs(0, cfg); got != 1000 {
		t.Fatalf("unknown duration should use unknown min, got %d", got)
	}
}

func TestRatePolicy(t *testing.T) {
	cfg := ViewPolicyConfig{MinDurationSec: 5}
	p, ok := RatePolicy(10, cfg)
	if !ok {
		t.Fatal("10s video should have rate policy")
	}
	if p.Capacity != 6 {
		t.Fatalf("capacity want 6, got %d", p.Capacity)
	}
	if p.LeakPerSecond != 0.1 {
		t.Fatalf("leak want 0.1/s, got %v", p.LeakPerSecond)
	}
	if _, ok := RatePolicy(4, cfg); ok {
		t.Fatal("video under 5s should skip bucket")
	}
	_, ok = RatePolicy(60, cfg)
	if !ok {
		t.Fatal("60s video should have policy")
	}
	p60, _ := RatePolicy(60, cfg)
	if p60.Capacity != 1 {
		t.Fatalf("60s => 1/min, capacity %d", p60.Capacity)
	}
	_ = ratelimit.Policy{} // compile-time import use
}
