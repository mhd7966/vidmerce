//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/like"
)

// TestLikeFlow_EndToEnd walks one like through every stage the production
// system would: the API-side Lua script, the Redis stream, the worker, and
// the resulting Postgres row + counter update.
//
//   1. POST-equivalent: service.Like()
//   2. stream:likes receives exactly one entry
//   3. worker.Run drains it and applies the change
//   4. likes table has the edge AND video_stats.likes_count == 1
//   5. service.Like() again is a no-op everywhere (idempotent)
func TestLikeFlow_EndToEnd(t *testing.T) {
	pool := pgClient(t)
	defer pool.Close()
	truncatePostgres(t, pool)

	rdb := redisClient(t)
	defer func() { _ = rdb.Close() }()

	vid, ownerID := seedVideo(t, pool)
	_ = ownerID
	uid := uuid.New()

	// Manually create the liker so the FK on likes.user_id succeeds.
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO users (id, email, password_hash)
		VALUES ($1, $2, 'x')
	`, uid, "liker+"+uid.String()+"@vidmerce.test"); err != nil {
		t.Fatalf("insert liker: %v", err)
	}

	repo := like.NewPostgresRepository(pool)
	svc := like.NewService(rdb, repo, quiet())

	// 1. Like — Redis side.
	st, err := svc.Like(context.Background(), uid, vid)
	if err != nil {
		t.Fatalf("like: %v", err)
	}
	if !st.Liked || st.Count != 1 {
		t.Fatalf("Redis state after like = %+v, want liked=true count=1", st)
	}

	// 2. The stream has exactly one entry.
	entries, err := rdb.XRange(context.Background(), "stream:likes", "-", "+").Result()
	if err != nil {
		t.Fatalf("xrange: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stream:likes len = %d, want 1", len(entries))
	}

	// 3. Run the worker just long enough to drain. We use a short context so
	// the worker exits after consuming the available entries.
	w := like.NewWorker(rdb, repo, quiet(), like.WorkerConfig{
		Consumer: "test-worker", BatchSize: 10, BlockTimeout: 200 * time.Millisecond,
	})
	runUntilDrained(t, w.Run, rdb, "stream:likes", "vidmerce-workers")

	// 4. Postgres now reflects the edge and the counter.
	var (
		edgeCount int64
		stat      int64
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM likes WHERE user_id = $1 AND video_id = $2`, uid, vid,
	).Scan(&edgeCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT likes_count FROM video_stats WHERE video_id = $1`, vid,
	).Scan(&stat); err != nil {
		t.Fatal(err)
	}
	if edgeCount != 1 || stat != 1 {
		t.Fatalf("postgres after worker: edge=%d count=%d, want 1,1", edgeCount, stat)
	}

	// 5. Idempotent re-like: Redis says noop, no new stream entry, Postgres
	// untouched.
	st2, err := svc.Like(context.Background(), uid, vid)
	if err != nil {
		t.Fatalf("re-like: %v", err)
	}
	if !st2.Liked || st2.Count != 1 {
		t.Fatalf("noop re-like state = %+v", st2)
	}
	// Either the stream still has 1 entry (the original; the noop did not
	// XADD), or 0 (if Redis trimmed during XADD MAXLEN ~). Both are valid.
	got, _ := rdb.XLen(context.Background(), "stream:likes").Result()
	if got > 1 {
		t.Fatalf("stream length grew to %d on a no-op like", got)
	}
}

// TestLikeReconciler_FixesDrift forces the Postgres counter out of sync with
// the actual likes table and proves the periodic reconciler heals it.
func TestLikeReconciler_FixesDrift(t *testing.T) {
	pool := pgClient(t)
	defer pool.Close()
	truncatePostgres(t, pool)

	vid, _ := seedVideo(t, pool)

	repo := like.NewPostgresRepository(pool)

	// Insert two real edges via the repo, then poison the counter to a wrong
	// value out-of-band.
	uid1, uid2 := uuid.New(), uuid.New()
	for _, uid := range []uuid.UUID{uid1, uid2} {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO users (id, email, password_hash)
			VALUES ($1, $2, 'x')
		`, uid, "u+"+uid.String()+"@vidmerce.test"); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Apply(context.Background(), uid, vid, like.OpLike); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE video_stats SET likes_count = 99 WHERE video_id = $1`, vid,
	); err != nil {
		t.Fatal(err)
	}

	rec := like.NewReconciler(repo, quiet(), like.ReconcilerConfig{SampleSize: 10})
	if err := rec.Once(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got int64
	if err := pool.QueryRow(context.Background(),
		`SELECT likes_count FROM video_stats WHERE video_id = $1`, vid,
	).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("counter after reconcile = %d, want 2", got)
	}
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
