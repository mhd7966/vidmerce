package like_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/like"
)

// fakeRepo is the in-memory test double used by both the reconciler and
// worker tests.
type fakeRepo struct {
	mu sync.Mutex
	// edges is the source of truth: (user, video) -> exists
	edges map[edgeKey]struct{}
	// counts is what video_stats currently stores; may drift if a test seeds it
	// out of sync deliberately.
	counts map[uuid.UUID]int64
	// updatedAt tracks the SampleVideoIDs ordering predicate.
	updatedAt map[uuid.UUID]time.Time

	// failures: optional injection points for tests.
	applyErr error
}

type edgeKey struct {
	uid uuid.UUID
	vid uuid.UUID
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		edges:     map[edgeKey]struct{}{},
		counts:    map[uuid.UUID]int64{},
		updatedAt: map[uuid.UUID]time.Time{},
	}
}

func (r *fakeRepo) seedVideo(vid uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.counts[vid]; !ok {
		r.counts[vid] = 0
		r.updatedAt[vid] = time.Time{}
	}
}

func (r *fakeRepo) seedDrift(vid uuid.UUID, wrong int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[vid] = wrong
	r.updatedAt[vid] = time.Time{}
}

func (r *fakeRepo) Apply(_ context.Context, uid, vid uuid.UUID, op like.Op) (like.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applyErr != nil {
		return like.Result{}, r.applyErr
	}
	if _, ok := r.counts[vid]; !ok {
		r.counts[vid] = 0
	}
	k := edgeKey{uid, vid}
	if op == like.OpLike {
		if _, exists := r.edges[k]; exists {
			return like.Result{Changed: false, NewCount: r.counts[vid], WasNoop: true}, nil
		}
		r.edges[k] = struct{}{}
		r.counts[vid]++
		r.updatedAt[vid] = time.Now()
		return like.Result{Changed: true, NewCount: r.counts[vid]}, nil
	}
	if _, exists := r.edges[k]; !exists {
		return like.Result{Changed: false, NewCount: r.counts[vid], WasNoop: true}, nil
	}
	delete(r.edges, k)
	if r.counts[vid] > 0 {
		r.counts[vid]--
	}
	r.updatedAt[vid] = time.Now()
	return like.Result{Changed: true, NewCount: r.counts[vid]}, nil
}

func (r *fakeRepo) Count(_ context.Context, vid uuid.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[vid], nil
}

func (r *fakeRepo) CountFromEdges(_ context.Context, vid uuid.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for k := range r.edges {
		if k.vid == vid {
			n++
		}
	}
	return n, nil
}

func (r *fakeRepo) ReconcileCounter(_ context.Context, vid uuid.UUID, exact int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts[vid] == exact {
		return 0, nil
	}
	r.counts[vid] = exact
	r.updatedAt[vid] = time.Now()
	return 1, nil
}

func (r *fakeRepo) SampleVideoIDs(_ context.Context, limit int) ([]uuid.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]uuid.UUID, 0, len(r.counts))
	for id := range r.counts {
		out = append(out, id)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// --- Reconciler tests --------------------------------------------------------

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReconcilerFixesDrift(t *testing.T) {
	r := newFakeRepo()
	vid := uuid.New()
	r.seedVideo(vid)

	// Add two real edges, but store the counter as 5 (drift by +3).
	if _, err := r.Apply(context.Background(), uuid.New(), vid, like.OpLike); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Apply(context.Background(), uuid.New(), vid, like.OpLike); err != nil {
		t.Fatal(err)
	}
	r.seedDrift(vid, 5)

	rec := like.NewReconciler(r, newSilentLogger(), like.ReconcilerConfig{
		Interval:   time.Hour, // we drive Once() manually
		SampleSize: 100,
	})
	if err := rec.Once(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got, _ := r.Count(context.Background(), vid)
	if got != 2 {
		t.Fatalf("drift not fixed: stored=%d want=2", got)
	}
}

func TestReconcilerNoOpWhenClean(t *testing.T) {
	r := newFakeRepo()
	vid := uuid.New()
	r.seedVideo(vid)
	if _, err := r.Apply(context.Background(), uuid.New(), vid, like.OpLike); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Count(context.Background(), vid)

	rec := like.NewReconciler(r, newSilentLogger(), like.ReconcilerConfig{
		SampleSize: 100,
	})
	if err := rec.Once(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after, _ := r.Count(context.Background(), vid)
	if before != after {
		t.Fatalf("clean count was rewritten: before=%d after=%d", before, after)
	}
}

func TestReconcilerCancellable(t *testing.T) {
	r := newFakeRepo()
	rec := like.NewReconciler(r, newSilentLogger(), like.ReconcilerConfig{
		Interval:   10 * time.Millisecond,
		SampleSize: 10,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconciler didn't stop after cancel")
	}
}

// --- Apply semantics ---------------------------------------------------------

func TestApplyIdempotent(t *testing.T) {
	r := newFakeRepo()
	uid, vid := uuid.New(), uuid.New()
	r.seedVideo(vid)

	first, err := r.Apply(context.Background(), uid, vid, like.OpLike)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.NewCount != 1 {
		t.Fatalf("first like: %+v", first)
	}
	second, err := r.Apply(context.Background(), uid, vid, like.OpLike)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed || !second.WasNoop || second.NewCount != 1 {
		t.Fatalf("duplicate like should be a no-op: %+v", second)
	}

	un1, err := r.Apply(context.Background(), uid, vid, like.OpUnlike)
	if err != nil {
		t.Fatal(err)
	}
	if !un1.Changed || un1.NewCount != 0 {
		t.Fatalf("unlike: %+v", un1)
	}
	un2, err := r.Apply(context.Background(), uid, vid, like.OpUnlike)
	if err != nil {
		t.Fatal(err)
	}
	if un2.Changed || !un2.WasNoop {
		t.Fatalf("duplicate unlike should be a no-op: %+v", un2)
	}
}
