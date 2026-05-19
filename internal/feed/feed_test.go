package feed_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/feed"
	"github.com/mhd7966/vidmerce/internal/video"
)

// --- Cursor codec ------------------------------------------------------------

func TestCursorRoundTrip(t *testing.T) {
	orig := feed.Cursor{
		CreatedAt: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
		ID:        uuid.New(),
	}
	s := orig.Encode()
	got, err := feed.DecodeCursor(s)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != orig.ID || !got.CreatedAt.Equal(orig.CreatedAt) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, orig)
	}
}

func TestDecodeCursorEmptyMeansStart(t *testing.T) {
	c, err := feed.DecodeCursor("")
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if !c.IsZero() {
		t.Fatal("empty cursor should decode to zero value")
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	cases := []string{"!!notbase64!!", "Zm9v" /* "foo" — valid base64, invalid JSON */}
	for _, c := range cases {
		if _, err := feed.DecodeCursor(c); !errors.Is(err, feed.ErrInvalidCursor) {
			t.Errorf("expected ErrInvalidCursor for %q, got %v", c, err)
		}
	}
}

// --- Pull fetcher ------------------------------------------------------------

// fakeSource is an in-memory video store sorted newest-first.
type fakeSource struct {
	videos []video.Video
}

func newFakeSource(n int) *fakeSource {
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	out := make([]video.Video, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, video.Video{
			ID:        uuid.New(),
			UserID:    uuid.New(),
			Title:     "v",
			VideoURL:  "https://x/v",
			CreatedAt: base.Add(time.Duration(i) * time.Hour),
		})
	}
	// newest-first
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return &fakeSource{videos: out}
}

func (f *fakeSource) FeedPage(_ context.Context, createdAt time.Time, id uuid.UUID, limit int) ([]video.Video, error) {
	start := 0
	if !(createdAt.IsZero() && id == uuid.Nil) {
		for i, v := range f.videos {
			if v.CreatedAt.Before(createdAt) ||
				(v.CreatedAt.Equal(createdAt) && v.ID.String() < id.String()) {
				start = i
				break
			}
			start = i + 1
		}
	}
	end := start + limit
	if end > len(f.videos) {
		end = len(f.videos)
	}
	return f.videos[start:end], nil
}

func (f *fakeSource) FindByIDs(_ context.Context, ids []uuid.UUID) ([]video.Video, error) {
	want := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	out := make([]video.Video, 0, len(ids))
	for _, v := range f.videos {
		if _, ok := want[v.ID]; ok {
			out = append(out, v)
		}
	}
	return out, nil
}

func TestPullExhaustsExactlyOnce(t *testing.T) {
	src := newFakeSource(7)
	p := feed.NewPullFetcher(src)
	ctx := context.Background()

	page1, err := p.Fetch(ctx, feed.Cursor{}, 3)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Items) != 3 {
		t.Fatalf("page1 size = %d, want 3", len(page1.Items))
	}
	if page1.NextCursor == "" {
		t.Fatal("page1 should have next cursor")
	}

	c, _ := feed.DecodeCursor(page1.NextCursor)
	page2, err := p.Fetch(ctx, c, 3)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Items) != 3 {
		t.Fatalf("page2 size = %d, want 3", len(page2.Items))
	}

	c, _ = feed.DecodeCursor(page2.NextCursor)
	page3, err := p.Fetch(ctx, c, 3)
	if err != nil {
		t.Fatalf("page3: %v", err)
	}
	if len(page3.Items) != 1 {
		t.Fatalf("page3 size = %d, want 1 (tail)", len(page3.Items))
	}
	if page3.NextCursor != "" {
		t.Fatalf("page3 should have no next cursor, got %q", page3.NextCursor)
	}

	// All three pages stitched together should cover the dataset exactly once.
	seen := map[uuid.UUID]int{}
	for _, p := range [][]video.Video{page1.Items, page2.Items, page3.Items} {
		for _, v := range p {
			seen[v.ID]++
		}
	}
	if len(seen) != 7 {
		t.Fatalf("expected 7 unique videos across pages, got %d", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("video %v seen %d times", id, n)
		}
	}
}

func TestPullEmptyDataset(t *testing.T) {
	p := feed.NewPullFetcher(newFakeSource(0))
	page, err := p.Fetch(context.Background(), feed.Cursor{}, 20)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("expected empty page, got %+v", page)
	}
}

func TestPullExactBoundary(t *testing.T) {
	// limit == total: must return all items with no NextCursor (we read
	// limit+1 internally; the absence of a (limit+1)th row signals end-of-feed).
	src := newFakeSource(5)
	p := feed.NewPullFetcher(src)

	page, err := p.Fetch(context.Background(), feed.Cursor{}, 5)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(page.Items) != 5 {
		t.Fatalf("got %d items, want 5", len(page.Items))
	}
	if page.NextCursor != "" {
		t.Fatalf("expected no next cursor at exact boundary, got %q", page.NextCursor)
	}
}
