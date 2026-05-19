//go:build integration

package integration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/stats"
)

// counting wrappers make it easy to assert "exactly N backend hits" without
// reaching into go-redis or ClickHouse internals.

type countingViews struct {
	delegate stats.ViewsCounter
	calls    int64
}

func (c *countingViews) Counts(ctx context.Context, vid uuid.UUID) (int64, int64, error) {
	atomic.AddInt64(&c.calls, 1)
	return c.delegate.Counts(ctx, vid)
}

type countingLikes struct {
	delegate stats.LikesCounter
	calls    int64
}

func (c *countingLikes) Count(ctx context.Context, vid uuid.UUID) (int64, error) {
	atomic.AddInt64(&c.calls, 1)
	return c.delegate.Count(ctx, vid)
}

// staticExister implements stats.VideoExister with a fixed answer — keeps
// the test focused on the cache + stampede paths.
type staticExister struct{ exists bool }

func (e *staticExister) Exists(context.Context, uuid.UUID) (bool, error) { return e.exists, nil }

// TestStats_CacheHitSkipsBackends asserts the most basic cache-aside
// guarantee: a second call with no state change hits neither backend.
func TestStats_CacheHitSkipsBackends(t *testing.T) {
	rdb := redisClient(t)
	defer func() { _ = rdb.Close() }()

	vid := uuid.New()

	// Use fakes so the test is fast and deterministic; the production
	// ClickHouse / Postgres backends are exercised by the views & likes
	// e2e tests above.
	views := &countingViews{delegate: fakeViews{views: 42, unique: 30}}
	likes := &countingLikes{delegate: fakeLikes{count: 7}}

	svc := stats.NewService(views, likes, &staticExister{exists: true}, rdb, quiet(), stats.Config{
		CacheTTL: 60 * time.Second, LockTTL: time.Second, LockRetry: 10 * time.Millisecond,
	})

	first, err := svc.Get(context.Background(), vid)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if first.Views != 42 || first.Likes != 7 || first.UniqueViews != 30 {
		t.Fatalf("first call payload: %+v", first)
	}
	if got := atomic.LoadInt64(&views.calls); got != 1 {
		t.Fatalf("views.calls after first call = %d, want 1", got)
	}

	second, err := svc.Get(context.Background(), vid)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if second.Views != first.Views || second.Likes != first.Likes {
		t.Fatalf("second call differs: %+v vs %+v", second, first)
	}
	// Cache hit — backends must NOT have been called again.
	if got := atomic.LoadInt64(&views.calls); got != 1 {
		t.Fatalf("views.calls after cache hit = %d, want 1", got)
	}
	if got := atomic.LoadInt64(&likes.calls); got != 1 {
		t.Fatalf("likes.calls after cache hit = %d, want 1", got)
	}
}

// TestStats_ConcurrentCallsCoalesce hammers the service with N concurrent
// requests for the same video on a cold cache and asserts that:
//
//   - All callers get the same answer.
//   - The backends are called at most once across all of them (singleflight
//     plus the distributed lock collapse the stampede).
//
// In a real deployment "at most once" is an upper bound that holds with a
// single replica; with multiple replicas the lock pushes us toward 1 but
// "twice" is possible during the lockRetry window if a holder dies. This
// test runs against one replica so the bound is exactly 1.
func TestStats_ConcurrentCallsCoalesce(t *testing.T) {
	rdb := redisClient(t)
	defer func() { _ = rdb.Close() }()

	vid := uuid.New()

	views := &countingViews{delegate: slowFakeViews{views: 100, unique: 50, delay: 50 * time.Millisecond}}
	likes := &countingLikes{delegate: slowFakeLikes{count: 10, delay: 50 * time.Millisecond}}

	svc := stats.NewService(views, likes, &staticExister{exists: true}, rdb, quiet(), stats.Config{
		CacheTTL: time.Minute, LockTTL: 5 * time.Second, LockRetry: 250 * time.Millisecond,
	})

	const N = 50
	var wg sync.WaitGroup
	results := make([]stats.Stats, N)
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i], errs[i] = svc.Get(context.Background(), vid)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < N; i++ {
		if results[i].Views != results[0].Views || results[i].Likes != results[0].Likes {
			t.Fatalf("inconsistent results: [0]=%+v [%d]=%+v", results[0], i, results[i])
		}
	}
	if got := atomic.LoadInt64(&views.calls); got > 1 {
		t.Fatalf("views.calls = %d under stampede; expected exactly 1 with singleflight + lock", got)
	}
	if got := atomic.LoadInt64(&likes.calls); got > 1 {
		t.Fatalf("likes.calls = %d under stampede; expected exactly 1", got)
	}
}

// TestStats_NotFoundShortCircuits asserts the 404 path skips both backends.
func TestStats_NotFoundShortCircuits(t *testing.T) {
	rdb := redisClient(t)
	defer func() { _ = rdb.Close() }()

	views := &countingViews{delegate: fakeViews{}}
	likes := &countingLikes{delegate: fakeLikes{}}
	svc := stats.NewService(views, likes, &staticExister{exists: false}, rdb, quiet(), stats.Config{})

	_, err := svc.Get(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("expected ErrVideoNotFound")
	}
	if atomic.LoadInt64(&views.calls) != 0 || atomic.LoadInt64(&likes.calls) != 0 {
		t.Fatalf("backends called on 404 path: views=%d likes=%d", views.calls, likes.calls)
	}
}

// --- in-test fakes (kept lean — no shared package needed) ---

type fakeViews struct{ views, unique int64 }

func (f fakeViews) Counts(context.Context, uuid.UUID) (int64, int64, error) {
	return f.views, f.unique, nil
}

type fakeLikes struct{ count int64 }

func (f fakeLikes) Count(context.Context, uuid.UUID) (int64, error) { return f.count, nil }

type slowFakeViews struct {
	views, unique int64
	delay         time.Duration
}

func (f slowFakeViews) Counts(ctx context.Context, _ uuid.UUID) (int64, int64, error) {
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return 0, 0, ctx.Err()
	}
	return f.views, f.unique, nil
}

type slowFakeLikes struct {
	count int64
	delay time.Duration
}

func (f slowFakeLikes) Count(ctx context.Context, _ uuid.UUID) (int64, error) {
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return f.count, nil
}
