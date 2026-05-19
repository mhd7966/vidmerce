package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/mhd7966/vidmerce/internal/auth"
	"github.com/mhd7966/vidmerce/internal/platform/jwt"
)

// --- In-memory fakes (no Postgres, no Redis) ---------------------------------

type fakeUserRepo struct {
	mu    sync.Mutex
	byID  map[uuid.UUID]auth.User
	byMail map[string]auth.User
}

func newFakeUserRepo() *fakeUserRepo {
	return &fakeUserRepo{
		byID:   map[uuid.UUID]auth.User{},
		byMail: map[string]auth.User{},
	}
}

func (r *fakeUserRepo) Create(_ context.Context, email, hash string) (auth.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byMail[email]; ok {
		return auth.User{}, auth.ErrEmailTaken
	}
	u := auth.User{
		ID:           uuid.New(),
		Email:        email,
		PasswordHash: hash,
		CreatedAt:    time.Now().UTC(),
	}
	r.byID[u.ID] = u
	r.byMail[email] = u
	return u, nil
}

func (r *fakeUserRepo) FindByEmail(_ context.Context, email string) (auth.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byMail[email]
	if !ok {
		return auth.User{}, auth.ErrUserNotFound
	}
	return u, nil
}

func (r *fakeUserRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byMail)
}

func (r *fakeUserRepo) FindByID(_ context.Context, id uuid.UUID) (auth.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[id]
	if !ok {
		return auth.User{}, auth.ErrUserNotFound
	}
	return u, nil
}

type fakeRefreshStore struct {
	mu     sync.Mutex
	tokens map[string]uuid.UUID
	seq    int
}

func newFakeRefreshStore() *fakeRefreshStore {
	return &fakeRefreshStore{tokens: map[string]uuid.UUID{}}
}

func (s *fakeRefreshStore) Issue(_ context.Context, uid uuid.UUID, _ time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	tok := uid.String() + ".secret-" + uuid.NewString()
	s.tokens[tok] = uid
	return tok, nil
}

func (s *fakeRefreshStore) Validate(_ context.Context, t string) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ok := s.tokens[t]
	if !ok {
		return uuid.Nil, auth.ErrInvalidRefresh
	}
	return uid, nil
}

func (s *fakeRefreshStore) Revoke(_ context.Context, t string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, t)
	return nil
}

// --- Test helpers -------------------------------------------------------------

func newSvc(t *testing.T) (*auth.Service, *fakeUserRepo, *fakeRefreshStore) {
	t.Helper()
	users := newFakeUserRepo()
	refresh := newFakeRefreshStore()
	j := jwt.NewService("test-secret", 15*time.Minute, "test")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Use bcrypt's minimum cost in tests to keep them fast.
	svc := auth.NewService(users, refresh, j, log, bcrypt.MinCost, time.Hour, nil)
	return svc, users, refresh
}

type fakeEmailBloom struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func newFakeEmailBloom() *fakeEmailBloom {
	return &fakeEmailBloom{m: map[string]struct{}{}}
}

func (f *fakeEmailBloom) MayContain(_ context.Context, member string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.m[member]
	return ok, nil
}

func (f *fakeEmailBloom) Add(_ context.Context, member string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[member] = struct{}{}
	return nil
}

// --- Tests --------------------------------------------------------------------

func TestRegisterRejectsBadInput(t *testing.T) {
	svc, _, _ := newSvc(t)
	ctx := context.Background()
	cases := []struct {
		name, email, password string
	}{
		{"empty email", "", "password123"},
		{"missing @", "alice.example.com", "password123"},
		{"short password", "alice@example.com", "short"},
		{"too-long password", "alice@example.com", string(make([]byte, 73))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Register(ctx, c.email, c.password)
			var v *auth.ValidationError
			if !errors.As(err, &v) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
		})
	}
}

func TestRegisterBloomRejectsBeforeDB(t *testing.T) {
	users := newFakeUserRepo()
	refresh := newFakeRefreshStore()
	j := jwt.NewService("test-secret", 15*time.Minute, "test")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	bf := newFakeEmailBloom()
	_ = bf.Add(context.Background(), auth.NormalizeEmail("taken@example.com"))

	svc := auth.NewService(users, refresh, j, log, bcrypt.MinCost, time.Hour, bf)
	_, err := svc.Register(context.Background(), "taken@example.com", "password123")
	if !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
	if users.count() != 0 {
		t.Fatal("bloom reject should not call repository Create")
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	svc, _, _ := newSvc(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "dup@example.com", "password123"); err != nil {
		t.Fatalf("first register: %v", err)
	}
	_, err := svc.Register(ctx, "dup@example.com", "password123")
	if !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken, got %v", err)
	}
}

func TestLoginHappyPath(t *testing.T) {
	svc, _, _ := newSvc(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "alice@example.com", "password123"); err != nil {
		t.Fatalf("register: %v", err)
	}
	tp, err := svc.Login(ctx, "alice@example.com", "password123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if tp.AccessToken == "" || tp.RefreshToken == "" {
		t.Fatalf("expected both tokens, got %+v", tp)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	svc, _, _ := newSvc(t)
	ctx := context.Background()
	_, _ = svc.Register(ctx, "alice@example.com", "password123")
	_, err := svc.Login(ctx, "alice@example.com", "wrong-password")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLoginUnknownEmailReturnsSameError(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Login(context.Background(), "nobody@example.com", "anything")
	if !errors.Is(err, auth.ErrInvalidCredentials) {
		// Critically, NOT ErrUserNotFound — leaking that would tell an attacker
		// whether an email is registered.
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestRefreshRotates(t *testing.T) {
	svc, _, store := newSvc(t)
	ctx := context.Background()
	_, _ = svc.Register(ctx, "alice@example.com", "password123")
	tp, _ := svc.Login(ctx, "alice@example.com", "password123")

	tp2, err := svc.Refresh(ctx, tp.RefreshToken)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tp2.RefreshToken == tp.RefreshToken {
		t.Fatal("refresh token should rotate")
	}

	// The old refresh token must no longer be accepted.
	if _, err := store.Validate(ctx, tp.RefreshToken); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("old refresh token still valid: %v", err)
	}
}

func TestLogoutRevokes(t *testing.T) {
	svc, _, store := newSvc(t)
	ctx := context.Background()
	_, _ = svc.Register(ctx, "alice@example.com", "password123")
	tp, _ := svc.Login(ctx, "alice@example.com", "password123")

	if err := svc.Logout(ctx, tp.RefreshToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := store.Validate(ctx, tp.RefreshToken); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Fatalf("token not revoked: %v", err)
	}
	// Logout is idempotent.
	if err := svc.Logout(ctx, tp.RefreshToken); err != nil {
		t.Fatalf("second logout should be noop: %v", err)
	}
}
