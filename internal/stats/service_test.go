package stats

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// --- Fakes -------------------------------------------------------------------

type fakeViews struct {
	mu          sync.Mutex
	views       int64
	unique      int64
	err         error
	delay       time.Duration
	callCount   int64
	lastVideoID uuid.UUID
}

func (f *fakeViews) Counts(ctx context.Context, vid uuid.UUID) (int64, int64, error) {
	atomic.AddInt64(&f.callCount, 1)
	f.mu.Lock()
	f.lastVideoID = vid
	d := f.delay
	err := f.err
	v, u := f.views, f.unique
	f.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		}
	}
	if err != nil {
		return 0, 0, err
	}
	return v, u, nil
}

type fakeLikes struct {
	mu        sync.Mutex
	count     int64
	err       error
	delay     time.Duration
	callCount int64
}

func (f *fakeLikes) Count(ctx context.Context, _ uuid.UUID) (int64, error) {
	atomic.AddInt64(&f.callCount, 1)
	f.mu.Lock()
	d := f.delay
	err := f.err
	c := f.count
	f.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return c, err
}

type fakeExister struct {
	exists bool
	err    error
}

func (f *fakeExister) Exists(context.Context, uuid.UUID) (bool, error) {
	return f.exists, f.err
}

// quietLogger silences logs in tests.
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newServiceForCompute builds a service whose Redis-touching helpers
// (readCache/writeCache/acquireLock/releaseLock) are bypassed by calling
// `compute` directly. The rdb field is nil because we never reach it.
func newServiceForCompute(views ViewsCounter, likes LikesCounter, exist VideoExister) *Service {
	return &Service{
		views: views, likes: likes, video: exist, log: quietLogger(),
		cacheTTL: time.Second, lockTTL: time.Second, lockRetry: time.Millisecond,
		clock: func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
}

// --- engagementRate ----------------------------------------------------------

func TestEngagementRate(t *testing.T) {
	cases := []struct {
		likes, unique int64
		want          float64
	}{
		{0, 0, 0},   // empty video
		{5, 0, 0},   // likes but no audience: divide-by-zero guard
		{1, 10, 0.1},
		{50, 100, 0.5},
	}
	for _, c := range cases {
		if got := engagementRate(c.likes, c.unique); got != c.want {
			t.Errorf("engagementRate(%d, %d) = %v, want %v", c.likes, c.unique, got, c.want)
		}
	}
}

// --- compute -----------------------------------------------------------------

func TestCompute_ParallelFetch(t *testing.T) {
	// Both backends sleep 50ms. If fetch runs them in parallel total time is
	// ~50ms; sequential would be ~100ms. Use 80ms as the cutoff.
	views := &fakeViews{views: 100, unique: 80, delay: 50 * time.Millisecond}
	likes := &fakeLikes{count: 20, delay: 50 * time.Millisecond}
	s := newServiceForCompute(views, likes, &fakeExister{exists: true})

	start := time.Now()
	got, err := s.compute(context.Background(), uuid.New())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("compute: %v", err)
	}
	if got.Views != 100 || got.UniqueViews != 80 || got.Likes != 20 {
		t.Fatalf("compute returned %+v", got)
	}
	if got.EngagementRate != 0.25 {
		t.Fatalf("engagement_rate = %v, want 0.25", got.EngagementRate)
	}
	if elapsed > 80*time.Millisecond {
		t.Fatalf("compute took %s; expected ~50ms parallel run", elapsed)
	}
}

func TestCompute_ViewsErrorIsPropagated(t *testing.T) {
	views := &fakeViews{err: errors.New("ch down")}
	likes := &fakeLikes{count: 1}
	s := newServiceForCompute(views, likes, &fakeExister{exists: true})

	_, err := s.compute(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error from compute")
	}
	if !errors.Is(err, err) || err.Error() == "" {
		t.Fatalf("error not wrapped: %v", err)
	}
}

func TestCompute_LikesErrorIsPropagated(t *testing.T) {
	views := &fakeViews{views: 1, unique: 1}
	likes := &fakeLikes{err: errors.New("pg down")}
	s := newServiceForCompute(views, likes, &fakeExister{exists: true})

	_, err := s.compute(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected error from compute")
	}
}

func TestCompute_CancelsParallelOnFirstError(t *testing.T) {
	// The slow goroutine should see its context cancelled when the fast one
	// errors first. We verify by timing: cancellation arrives well before the
	// slow goroutine's natural delay.
	views := &fakeViews{err: errors.New("immediate fail")} // returns instantly
	likes := &fakeLikes{count: 1, delay: 500 * time.Millisecond}
	s := newServiceForCompute(views, likes, &fakeExister{exists: true})

	start := time.Now()
	_, err := s.compute(context.Background(), uuid.New())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("did not cancel slow goroutine on first error; took %s", elapsed)
	}
}

// --- ComputedAt --------------------------------------------------------------

func TestCompute_ComputedAtUsesClock(t *testing.T) {
	s := newServiceForCompute(&fakeViews{}, &fakeLikes{}, &fakeExister{exists: true})
	got, err := s.compute(context.Background(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if !got.ComputedAt.Equal(time.Unix(1700000000, 0).UTC()) {
		t.Fatalf("ComputedAt = %s; want injected clock value", got.ComputedAt)
	}
}
