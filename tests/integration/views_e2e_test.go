//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/ratelimit"
	"github.com/mhd7966/vidmerce/internal/view"
)

// TestViewFlow_EndToEnd drives views through the new model: replays count as
// total views; unique_views increments once per unique window.
func TestViewFlow_EndToEnd(t *testing.T) {
	pool := pgClient(t)
	defer pool.Close()
	truncatePostgres(t, pool)

	rdb := redisClient(t)
	defer func() { _ = rdb.Close() }()

	ch := chConn(t)
	defer func() { _ = ch.Close() }()
	truncateClickHouse(t, ch)

	const durationSec = 30
	vid := seedVideoWithDuration(t, pool, rdb, durationSec)
	uid := uuid.New()
	in := view.Input{
		VideoID:  vid,
		ViewerID: &uid,
		IPHash:   view.HashIP("203.0.113.42"),
		UAHash:   view.HashUA("Mozilla/5.0"),
		Country:  "US",
		WatchMs:  10000, // ≥ duration/3
	}

	viewPolicy := view.ViewPolicyConfig{MinDurationSec: 5, UnknownMinWatchMs: 1000}
	bucket := ratelimit.New(rdb)
	chain := view.NewChain(quiet(),
		view.NewWatchThresholdFilter(viewPolicy),
		view.NewDurationRateFilter(bucket, viewPolicy, quiet(), false),
	)
	unique := view.NewUniqueMarker(rdb, 10*time.Minute)
	durStore := view.NewRedisDurationStore(rdb, time.Hour)
	svc := view.NewService(rdb, chain, durStore, unique, quiet())

	res, err := svc.Track(context.Background(), in)
	if err != nil {
		t.Fatalf("track: %v", err)
	}
	if !res.Accepted || !res.IsUnique {
		t.Fatalf("first view: accepted=%v unique=%v", res.Accepted, res.IsUnique)
	}

	res2, err := svc.Track(context.Background(), in)
	if err != nil {
		t.Fatalf("track2: %v", err)
	}
	if !res2.Accepted {
		t.Fatalf("replay should count as total view, rejected_by=%s", res2.RejectedBy)
	}
	if res2.IsUnique {
		t.Fatal("replay within unique TTL should not be unique")
	}
	if n, _ := rdb.XLen(context.Background(), "stream:views").Result(); n != 2 {
		t.Fatalf("stream:views len = %d, want 2", n)
	}

	sink := view.NewClickHouseSink(ch, quiet())
	w := view.NewWorker(rdb, sink, quiet(), view.WorkerConfig{
		Consumer: "test-view-worker", FlushSize: 10, FlushInterval: 100 * time.Millisecond,
	})
	runUntilDrained(t, w.Run, rdb, "stream:views", "vidmerce-workers")

	waitFor(t, 5*time.Second, "video_views rows", func() bool {
		var n uint64
		if err := ch.QueryRow(context.Background(),
			`SELECT count() FROM video_views WHERE video_id = ?`, vid,
		).Scan(&n); err != nil {
			return false
		}
		return n == 2
	})

	waitFor(t, 5*time.Second, "video_views_daily rollup", func() bool {
		var views, unique uint64
		if err := ch.QueryRow(context.Background(),
			`SELECT sum(views), sum(unique_views) FROM video_views_daily WHERE video_id = ?`, vid,
		).Scan(&views, &unique); err != nil {
			return false
		}
		return views == 2 && unique == 1
	})
}

// TestViewFilters_ShortCircuit asserts watch threshold rejects before Redis rate keys.
func TestViewFilters_ShortCircuit(t *testing.T) {
	rdb := redisClient(t)
	defer func() { _ = rdb.Close() }()

	vid := uuid.New()
	_ = view.NewRedisDurationStore(rdb, time.Hour).Set(context.Background(), vid, 30)

	viewPolicy := view.ViewPolicyConfig{MinDurationSec: 5, UnknownMinWatchMs: 1000}
	chain := view.NewChain(quiet(),
		view.NewWatchThresholdFilter(viewPolicy),
		view.NewDurationRateFilter(ratelimit.New(rdb), viewPolicy, quiet(), false),
	)
	svc := view.NewService(rdb, chain, view.NewRedisDurationStore(rdb, time.Hour), nil, quiet())

	uid := uuid.New()
	res, err := svc.Track(context.Background(), view.Input{
		VideoID:  vid,
		ViewerID: &uid,
		IPHash:   view.HashIP("198.51.100.1"),
		UAHash:   view.HashUA("ua"),
		WatchMs:  10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted {
		t.Fatal("watch_ms=10 should be rejected for 30s video")
	}
	if res.RejectedBy != "watch_threshold:below_threshold" {
		t.Fatalf("rejectedBy = %q", res.RejectedBy)
	}
}
