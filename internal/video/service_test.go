package video_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/video"
)

// --- Fakes -------------------------------------------------------------------

type fakeRepo struct {
	mu     sync.Mutex
	byID   map[uuid.UUID]video.Video
}

func newFakeRepo() *fakeRepo { return &fakeRepo{byID: map[uuid.UUID]video.Video{}} }

func (r *fakeRepo) Create(_ context.Context, uid uuid.UUID, in video.CreateInput) (video.Video, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dur := in.DurationSec
	if dur <= 0 {
		dur = 30
	}
	v := video.Video{
		ID:          uuid.New(),
		UserID:      uid,
		Title:       in.Title,
		Description: in.Description,
		VideoURL:    in.VideoURL,
		DurationSec: dur,
		CreatedAt:   time.Now().UTC(),
	}
	r.byID[v.ID] = v
	return v, nil
}

func (r *fakeRepo) FindByID(_ context.Context, id uuid.UUID) (video.Video, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byID[id]
	if !ok {
		return video.Video{}, video.ErrVideoNotFound
	}
	return v, nil
}

func (r *fakeRepo) FindByIDs(_ context.Context, ids []uuid.UUID) ([]video.Video, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]video.Video, 0, len(ids))
	for _, id := range ids {
		if v, ok := r.byID[id]; ok {
			out = append(out, v)
		}
	}
	return out, nil
}

func (r *fakeRepo) FeedPage(_ context.Context, createdAt time.Time, id uuid.UUID, limit int) ([]video.Video, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]video.Video, 0, len(r.byID))
	for _, v := range r.byID {
		out = append(out, v)
	}
	// newest-first
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	// keyset filter
	if !(createdAt.IsZero() && id == uuid.Nil) {
		start := 0
		for i, v := range out {
			if v.CreatedAt.Before(createdAt) ||
				(v.CreatedAt.Equal(createdAt) && v.ID.String() < id.String()) {
				start = i
				break
			}
			start = i + 1
		}
		out = out[start:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type fakeCache struct {
	mu   sync.Mutex
	m    map[string]video.Video
	hits int
	gets int
}

func newFakeCache() *fakeCache { return &fakeCache{m: map[string]video.Video{}} }

func (c *fakeCache) Get(_ context.Context, id string) (video.Video, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	v, ok := c.m[id]
	if ok {
		c.hits++
	}
	return v, ok, nil
}
func (c *fakeCache) Set(_ context.Context, id string, v video.Video) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[id] = v
	return nil
}
func (c *fakeCache) Del(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, id)
	return nil
}

// --- Helpers -----------------------------------------------------------------

func newSvc(t *testing.T, withCache bool) (*video.Service, *fakeRepo, *fakeCache) {
	t.Helper()
	repo := newFakeRepo()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var c *fakeCache
	if withCache {
		c = newFakeCache()
		return video.NewService(repo, c, nil, log), repo, c
	}
	return video.NewService(repo, nil, nil, log), repo, nil
}

func validInput() video.CreateInput {
	return video.CreateInput{
		Title:        "hello",
		Description:  "world",
		VideoURL:     "https://cdn.example.com/v/1.mp4",
		DurationSec:  30,
	}
}

// --- Tests -------------------------------------------------------------------

func TestCreateAndGet(t *testing.T) {
	svc, _, _ := newSvc(t, false)
	ctx := context.Background()

	v, err := svc.Create(ctx, uuid.New(), validInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := svc.Get(ctx, v.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != v {
		t.Fatalf("round-trip mismatch: %+v vs %+v", got, v)
	}
}

func TestCreateValidation(t *testing.T) {
	svc, _, _ := newSvc(t, false)
	ctx := context.Background()

	cases := []struct {
		name string
		in   video.CreateInput
	}{
		{"empty title", video.CreateInput{Title: "  ", VideoURL: "https://x.example/v"}},
		{"missing url", video.CreateInput{Title: "ok", VideoURL: ""}},
		{"bad url scheme", video.CreateInput{Title: "ok", VideoURL: "ftp://x/v"}},
		{"oversize title", video.CreateInput{
			Title: string(make([]byte, 201)), VideoURL: "https://x.example/v"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Create(ctx, uuid.New(), c.in)
			var v *video.ValidationError
			if !errors.As(err, &v) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
		})
	}
}

func TestGetMissing(t *testing.T) {
	svc, _, _ := newSvc(t, false)
	_, err := svc.Get(context.Background(), uuid.New())
	if !errors.Is(err, video.ErrVideoNotFound) {
		t.Fatalf("expected ErrVideoNotFound, got %v", err)
	}
}

func TestCacheHitSkipsRepo(t *testing.T) {
	svc, repo, c := newSvc(t, true)
	ctx := context.Background()

	v, err := svc.Create(ctx, uuid.New(), validInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Remove from the repo behind the cache's back. A subsequent Get must
	// still return the cached value, confirming the cache is being consulted.
	repo.mu.Lock()
	delete(repo.byID, v.ID)
	repo.mu.Unlock()

	got, err := svc.Get(ctx, v.ID)
	if err != nil {
		t.Fatalf("get from cache: %v", err)
	}
	if got != v {
		t.Fatalf("got %+v want %+v", got, v)
	}
	if c.hits == 0 {
		t.Fatal("expected at least one cache hit")
	}
}

func TestAssertOwner(t *testing.T) {
	svc, _, _ := newSvc(t, false)
	ctx := context.Background()
	owner := uuid.New()
	other := uuid.New()

	v, err := svc.Create(ctx, owner, validInput())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.AssertOwner(ctx, v.ID, owner); err != nil {
		t.Fatalf("owner asserted as non-owner: %v", err)
	}
	if err := svc.AssertOwner(ctx, v.ID, other); !errors.Is(err, video.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
	if err := svc.AssertOwner(ctx, uuid.New(), owner); !errors.Is(err, video.ErrVideoNotFound) {
		t.Fatalf("expected ErrVideoNotFound, got %v", err)
	}
}
