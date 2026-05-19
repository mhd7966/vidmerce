// Package auth implements user lifecycle (register, login, refresh, logout)
// and exposes a Gin handler that wires those operations to HTTP. It owns the
// `users` table in Postgres and refresh-token state in Redis.
//
// What the package does *not* own:
//   - Token signing / parsing — that's internal/platform/jwt.
//   - Rate limiting — that's internal/platform/ratelimit.
//   - Password hashing — bcrypt is called inline here because the salt and
//     cost are tightly coupled to the User domain and aren't reused elsewhere.
package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// NormalizeEmail lowercases and trims an email, matching Postgres storage
// (CITEXT + repository normalisation). Bloom filters and UNIQUE checks must
// use the same form.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Domain errors. The handler maps these to HTTP statuses; the service never
// returns raw database errors to the caller.
var (
	ErrEmailTaken         = errors.New("email already registered")
	ErrInvalidCredentials = errors.New("invalid email or password")
	ErrUserNotFound       = errors.New("user not found")
	ErrInvalidRefresh     = errors.New("invalid or expired refresh token")
)

// ValidationError carries a user-visible message describing what was wrong
// with the request. Wrap it with fmt.Errorf("%w: ...", ErrValidation) to
// preserve the marker.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// ErrValidation is the sentinel used with errors.Is to detect *any*
// ValidationError. We expose both because handlers may want to detect the
// general case while logs sometimes want the specific message.
var ErrValidation = &ValidationError{Msg: "validation failed"}

// Is satisfies errors.Is so any *ValidationError matches ErrValidation.
func (e *ValidationError) Is(target error) bool {
	_, ok := target.(*ValidationError)
	return ok
}

// User is the canonical user record. PasswordHash is exposed inside the
// package boundary only; handler responses never include it.
type User struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

// PublicUser is the safe-to-serialise view of a User. The service hands these
// to the handler so accidental leaking of PasswordHash is structurally
// impossible.
type PublicUser struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// Public converts a User into its safe view.
func (u User) Public() PublicUser {
	return PublicUser{ID: u.ID, Email: u.Email, CreatedAt: u.CreatedAt}
}

// TokenPair is what /auth/login and /auth/refresh return. AccessExpiresAt is
// included so clients don't have to decode the JWT to know when to refresh.
type TokenPair struct {
	AccessToken     string    `json:"access_token"`
	RefreshToken    string    `json:"refresh_token"`
	AccessExpiresAt time.Time `json:"access_expires_at"`
	TokenType       string    `json:"token_type"` // always "Bearer"
}
