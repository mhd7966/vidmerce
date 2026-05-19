package product_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/product"
	"github.com/mhd7966/vidmerce/internal/video"
)

// --- Fakes -------------------------------------------------------------------

type fakeRepo struct {
	mu      sync.Mutex
	byID    map[uuid.UUID]product.Product
	byVideo map[uuid.UUID]product.Product
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		byID:    map[uuid.UUID]product.Product{},
		byVideo: map[uuid.UUID]product.Product{},
	}
}

func (r *fakeRepo) Create(_ context.Context, in product.CreateInput) (product.Product, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byVideo[in.VideoID]; ok {
		return product.Product{}, product.ErrVideoAlreadyTaken
	}
	p := product.Product{
		ID:         uuid.New(),
		VideoID:    in.VideoID,
		Name:       in.Name,
		PriceCents: in.PriceCents,
		Currency:   in.Currency,
		ImageURL:   in.ImageURL,
		CreatedAt:  time.Now().UTC(),
	}
	r.byID[p.ID] = p
	r.byVideo[p.VideoID] = p
	return p, nil
}

func (r *fakeRepo) FindByID(_ context.Context, id uuid.UUID) (product.Product, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return product.Product{}, product.ErrProductNotFound
	}
	return p, nil
}

func (r *fakeRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byVideo)
}

func (r *fakeRepo) FindByVideoID(_ context.Context, vid uuid.UUID) (product.Product, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byVideo[vid]
	if !ok {
		return product.Product{}, product.ErrProductNotFound
	}
	return p, nil
}

// fakeAsserter implements VideoOwnerAsserter without touching a real video service.
type fakeAsserter struct {
	known map[uuid.UUID]uuid.UUID // videoID -> ownerID
}

func (f *fakeAsserter) AssertOwner(_ context.Context, videoID, userID uuid.UUID) error {
	owner, ok := f.known[videoID]
	if !ok {
		return video.ErrVideoNotFound
	}
	if owner != userID {
		return video.ErrForbidden
	}
	return nil
}

// --- Helpers -----------------------------------------------------------------

func newSvc(t *testing.T) (*product.Service, *fakeRepo, *fakeAsserter) {
	t.Helper()
	repo := newFakeRepo()
	as := &fakeAsserter{known: map[uuid.UUID]uuid.UUID{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return product.NewService(repo, nil, nil, as, log, nil), repo, as
}

type fakeVideoBloom struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func newFakeVideoBloom() *fakeVideoBloom {
	return &fakeVideoBloom{m: map[string]struct{}{}}
}

func (f *fakeVideoBloom) MayContain(_ context.Context, member string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.m[member]
	return ok, nil
}

func (f *fakeVideoBloom) Add(_ context.Context, member string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[member] = struct{}{}
	return nil
}

func validInput(videoID uuid.UUID) product.CreateInput {
	return product.CreateInput{
		VideoID:    videoID,
		Name:       "Hoodie",
		PriceCents: 1999,
		Currency:   "USD",
		ImageURL:   "https://cdn.example.com/p/1.png",
	}
}

// --- Tests -------------------------------------------------------------------

func TestCreateHappyPath(t *testing.T) {
	svc, _, as := newSvc(t)
	owner := uuid.New()
	vid := uuid.New()
	as.known[vid] = owner

	p, err := svc.Create(context.Background(), owner, validInput(vid))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.VideoID != vid {
		t.Fatalf("video id mismatch: got %v want %v", p.VideoID, vid)
	}
}

func TestCreateRejectsNonOwner(t *testing.T) {
	svc, _, as := newSvc(t)
	owner := uuid.New()
	intruder := uuid.New()
	vid := uuid.New()
	as.known[vid] = owner

	_, err := svc.Create(context.Background(), intruder, validInput(vid))
	if !errors.Is(err, video.ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}
}

func TestCreateRejectsMissingVideo(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Create(context.Background(), uuid.New(), validInput(uuid.New()))
	if !errors.Is(err, video.ErrVideoNotFound) {
		t.Fatalf("expected ErrVideoNotFound, got %v", err)
	}
}

func TestCreateBloomRejectsBeforeDB(t *testing.T) {
	repo := newFakeRepo()
	owner := uuid.New()
	vid := uuid.New()
	as := &fakeAsserter{known: map[uuid.UUID]uuid.UUID{vid: owner}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bf := newFakeVideoBloom()
	_ = bf.Add(context.Background(), vid.String())

	svc := product.NewService(repo, nil, nil, as, log, bf)
	_, err := svc.Create(context.Background(), owner, validInput(vid))
	if !errors.Is(err, product.ErrVideoAlreadyTaken) {
		t.Fatalf("expected ErrVideoAlreadyTaken, got %v", err)
	}
	if repo.count() != 0 {
		t.Fatal("bloom reject should not call repository Create")
	}
}

func TestCreateDuplicateOnSameVideo(t *testing.T) {
	svc, _, as := newSvc(t)
	owner := uuid.New()
	vid := uuid.New()
	as.known[vid] = owner

	if _, err := svc.Create(context.Background(), owner, validInput(vid)); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.Create(context.Background(), owner, validInput(vid))
	if !errors.Is(err, product.ErrVideoAlreadyTaken) {
		t.Fatalf("expected ErrVideoAlreadyTaken, got %v", err)
	}
}

func TestCreateValidation(t *testing.T) {
	svc, _, as := newSvc(t)
	owner := uuid.New()
	vid := uuid.New()
	as.known[vid] = owner

	cases := []struct {
		name string
		in   product.CreateInput
	}{
		{"empty name", product.CreateInput{VideoID: vid, Name: " ", PriceCents: 100, Currency: "USD", ImageURL: "https://x/x"}},
		{"negative price", product.CreateInput{VideoID: vid, Name: "ok", PriceCents: -1, Currency: "USD", ImageURL: "https://x/x"}},
		{"bad currency", product.CreateInput{VideoID: vid, Name: "ok", PriceCents: 1, Currency: "DOLLARS", ImageURL: "https://x/x"}},
		{"bad image url", product.CreateInput{VideoID: vid, Name: "ok", PriceCents: 1, Currency: "USD", ImageURL: "javascript:alert(1)"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Create(context.Background(), owner, c.in)
			var v *product.ValidationError
			if !errors.As(err, &v) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
		})
	}
}

func TestGetByIDAndByVideoID(t *testing.T) {
	svc, _, as := newSvc(t)
	owner := uuid.New()
	vid := uuid.New()
	as.known[vid] = owner

	p, err := svc.Create(context.Background(), owner, validInput(vid))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got1, err := svc.GetByID(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	got2, err := svc.GetByVideoID(context.Background(), vid)
	if err != nil {
		t.Fatalf("get by video id: %v", err)
	}
	if got1.ID != p.ID || got2.ID != p.ID {
		t.Fatal("read paths disagree on identity")
	}
}
