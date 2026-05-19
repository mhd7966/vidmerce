package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	platformjwt "github.com/mhd7966/vidmerce/internal/platform/jwt"
	"github.com/mhd7966/vidmerce/internal/platform/bloom"
)

// Service encapsulates the business logic of authentication. It depends only
// on interfaces (UserRepository, RefreshTokenStore) and a JWT signer; no Gin,
// no SQL, no Redis client appears here, which makes the unit tests trivial.
type Service struct {
	users      UserRepository
	refresh    RefreshTokenStore
	jwt        *platformjwt.Service
	log        *slog.Logger
	bcryptCost int
	refreshTTL time.Duration
	emails     bloom.Filter // optional Redis Bloom; nil disables fast reject
}

// NewService wires the auth service. bcryptCost defaults to bcrypt.DefaultCost
// when zero; refreshTTL defaults to 7 days.
func NewService(
	users UserRepository,
	refresh RefreshTokenStore,
	jwt *platformjwt.Service,
	log *slog.Logger,
	bcryptCost int,
	refreshTTL time.Duration,
	emailBloom bloom.Filter,
) *Service {
	if bcryptCost <= 0 {
		bcryptCost = bcrypt.DefaultCost
	}
	if refreshTTL <= 0 {
		refreshTTL = 7 * 24 * time.Hour
	}
	return &Service{
		users:      users,
		refresh:    refresh,
		jwt:        jwt,
		log:        log,
		bcryptCost: bcryptCost,
		refreshTTL: refreshTTL,
		emails:     emailBloom,
	}
}

// Register creates a new account. Email is normalised (trimmed + lower-cased)
// in the repository to avoid duplicates-with-different-case slipping through.
func (s *Service) Register(ctx context.Context, email, password string) (PublicUser, error) {
	if err := validateEmail(email); err != nil {
		return PublicUser{}, err
	}
	if err := validatePassword(password); err != nil {
		return PublicUser{}, err
	}
	norm := NormalizeEmail(email)
	if dup, err := s.emailMaybeTaken(ctx, norm); err != nil {
		return PublicUser{}, err
	} else if dup {
		return PublicUser{}, ErrEmailTaken
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.bcryptCost)
	if err != nil {
		return PublicUser{}, fmt.Errorf("hash password: %w", err)
	}
	u, err := s.users.Create(ctx, email, string(hash))
	if err != nil {
		if errors.Is(err, ErrEmailTaken) {
			s.recordEmail(ctx, norm)
		}
		return PublicUser{}, err
	}
	s.recordEmail(ctx, norm)
	s.log.Info("user registered", slog.String("user_id", u.ID.String()))
	return u.Public(), nil
}

func (s *Service) emailMaybeTaken(ctx context.Context, normEmail string) (bool, error) {
	if s.emails == nil {
		return false, nil
	}
	maybe, err := s.emails.MayContain(ctx, normEmail)
	if err != nil {
		s.log.Warn("email bloom check failed; falling back to db", slog.Any("error", err))
		return false, nil
	}
	return maybe, nil
}

func (s *Service) recordEmail(ctx context.Context, normEmail string) {
	if s.emails == nil {
		return
	}
	if err := s.emails.Add(ctx, normEmail); err != nil {
		s.log.Warn("email bloom add failed", slog.Any("error", err))
	}
}

// Login verifies credentials and issues a fresh access + refresh token pair.
//
// On invalid email OR invalid password we return the same ErrInvalidCredentials
// so attackers can't distinguish "this email exists" from "this email doesn't
// exist" by timing or by error code.
func (s *Service) Login(ctx context.Context, email, password string) (TokenPair, error) {
	u, err := s.users.FindByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// Run a dummy compare so the request time is similar to a real one.
			_ = bcrypt.CompareHashAndPassword(
				[]byte("$2a$12$0000000000000000000000.00000000000000000000000000000000"),
				[]byte(password),
			)
			return TokenPair{}, ErrInvalidCredentials
		}
		return TokenPair{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return TokenPair{}, ErrInvalidCredentials
	}
	return s.issueTokens(ctx, u.ID)
}

// Refresh exchanges a valid refresh token for a new access+refresh pair, and
// invalidates the old refresh token (rotation). Stealing a single refresh
// token therefore gives an attacker at most one access-token lifetime, not
// indefinite access.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	uid, err := s.refresh.Validate(ctx, refreshToken)
	if err != nil {
		return TokenPair{}, ErrInvalidRefresh
	}
	if revErr := s.refresh.Revoke(ctx, refreshToken); revErr != nil {
		s.log.Warn("refresh revoke failed; continuing", slog.Any("error", revErr))
	}
	return s.issueTokens(ctx, uid)
}

// Logout revokes the presented refresh token. Idempotent: revoking an unknown
// token is a success.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if err := s.refresh.Revoke(ctx, refreshToken); err != nil {
		return fmt.Errorf("revoke refresh: %w", err)
	}
	return nil
}

func (s *Service) issueTokens(ctx context.Context, uid uuid.UUID) (TokenPair, error) {
	access, expiresAt, err := s.jwt.IssueAccess(uid)
	if err != nil {
		return TokenPair{}, fmt.Errorf("issue access: %w", err)
	}
	refresh, err := s.refresh.Issue(ctx, uid, s.refreshTTL)
	if err != nil {
		return TokenPair{}, fmt.Errorf("issue refresh: %w", err)
	}
	return TokenPair{
		AccessToken:     access,
		RefreshToken:    refresh,
		AccessExpiresAt: expiresAt,
		TokenType:       "Bearer",
	}, nil
}

// --- validators (intentionally lightweight) ----------------------------------

func validateEmail(email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return &ValidationError{Msg: "email is required"}
	}
	if !strings.Contains(email, "@") || strings.Contains(email, " ") {
		return &ValidationError{Msg: "invalid email"}
	}
	if len(email) > 254 {
		return &ValidationError{Msg: "email too long"}
	}
	return nil
}

func validatePassword(pw string) error {
	if len(pw) < 8 {
		return &ValidationError{Msg: "password must be at least 8 characters"}
	}
	if len(pw) > 72 {
		// bcrypt only hashes the first 72 bytes; longer passwords silently
		// collide on prefix. Reject explicitly so users aren't misled.
		return &ValidationError{Msg: "password must be at most 72 characters"}
	}
	return nil
}
