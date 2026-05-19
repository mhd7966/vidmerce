package view_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/view"
)

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// staticFilter is a tiny test double: returns the configured (allowed,
// reason) tuple regardless of input and records how many times it was hit.
type staticFilter struct {
	name    string
	allowed bool
	reason  string
	calls   int
}

func (s *staticFilter) Name() string { return s.name }
func (s *staticFilter) Allow(context.Context, view.Input) (bool, string) {
	s.calls++
	return s.allowed, s.reason
}

func TestChainShortCircuits(t *testing.T) {
	a := &staticFilter{name: "a", allowed: true}
	b := &staticFilter{name: "b", allowed: false, reason: "blocked"}
	c := &staticFilter{name: "c", allowed: true}
	chain := view.NewChain(silentLogger(), a, b, c)

	in := view.Input{VideoID: uuid.New()}
	ok, rb := chain.Apply(context.Background(), in)
	if ok {
		t.Fatal("expected reject")
	}
	if rb != "b:blocked" {
		t.Fatalf("rejectedBy = %q, want b:blocked", rb)
	}
	if a.calls != 1 || b.calls != 1 || c.calls != 0 {
		t.Fatalf("call counts: a=%d b=%d c=%d (c must not run after b rejects)", a.calls, b.calls, c.calls)
	}
}

func TestChainAllPass(t *testing.T) {
	a := &staticFilter{name: "a", allowed: true}
	b := &staticFilter{name: "b", allowed: true}
	chain := view.NewChain(silentLogger(), a, b)
	ok, rb := chain.Apply(context.Background(), view.Input{VideoID: uuid.New()})
	if !ok {
		t.Fatalf("expected allow, rejectedBy=%q", rb)
	}
	if rb != "" {
		t.Fatalf("rejectedBy must be empty on allow, got %q", rb)
	}
}

func TestWatchThresholdOneThird(t *testing.T) {
	f := view.NewWatchThresholdFilter(view.ViewPolicyConfig{MinDurationSec: 5})
	in := view.Input{VideoID: uuid.New(), DurationSec: 30, WatchMs: 9999}
	if ok, _ := f.Allow(context.Background(), in); ok {
		t.Fatal("9999ms < 10000ms required for 30s video")
	}
	in.WatchMs = 10000
	if ok, _ := f.Allow(context.Background(), in); !ok {
		t.Fatal("10000ms should pass for 30s video")
	}
}

func TestMinWatchTimeFilter(t *testing.T) {
	f := view.NewMinWatchTimeFilter(1000)
	in := view.Input{VideoID: uuid.New(), WatchMs: 999}
	if ok, _ := f.Allow(context.Background(), in); ok {
		t.Fatal("999ms should be rejected at threshold 1000")
	}
	in.WatchMs = 1000
	if ok, _ := f.Allow(context.Background(), in); !ok {
		t.Fatal("at-threshold should pass")
	}
}

func TestMinWatchTimeFilterDisabled(t *testing.T) {
	f := view.NewMinWatchTimeFilter(0)
	in := view.Input{VideoID: uuid.New(), WatchMs: 0}
	if ok, _ := f.Allow(context.Background(), in); !ok {
		t.Fatal("threshold=0 must disable the filter")
	}
}

func TestSubjectKey(t *testing.T) {
	uid := uuid.New()
	withUser := view.Input{
		VideoID:  uuid.New(),
		ViewerID: &uid,
		IPHash:   "11111111111111111111111111111111",
		UAHash:   "1111111111111111",
	}
	if got := withUser.SubjectKey(); got != "u:"+uid.String() {
		t.Fatalf("logged-in subject = %q", got)
	}

	anon := view.Input{
		VideoID: uuid.New(),
		IPHash:  "22222222222222222222222222222222",
		UAHash:  "2222222222222222",
	}
	want := "a:" + anon.IPHash + ":" + anon.UAHash
	if got := anon.SubjectKey(); got != want {
		t.Fatalf("anon subject = %q want %q", got, want)
	}
}

func TestHashLengths(t *testing.T) {
	if got := len(view.HashIP("203.0.113.42")); got != 32 {
		t.Fatalf("HashIP returned %d chars, want 32", got)
	}
	if got := len(view.HashUA("Mozilla/5.0")); got != 16 {
		t.Fatalf("HashUA returned %d chars, want 16", got)
	}
	if view.HashIP("") != "00000000000000000000000000000000" {
		t.Fatal("HashIP empty must be zeroed sentinel")
	}
}
