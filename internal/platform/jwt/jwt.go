// Package jwt issues and verifies short-lived access tokens. Refresh tokens
// are *not* JWTs in this project — they are opaque random strings whose state
// (revoked / not revoked) lives in Redis. See internal/auth/refresh.go.
//
// We pick HS256 over RS256 deliberately:
//
//   - The token issuer and verifier are the same service.
//   - HS256 is faster (one HMAC vs RSA verify).
//   - The secret never leaves the trust boundary.
//
// If we ever split issuance and verification across services (e.g. an API
// gateway pattern) we revisit this and move to RS256/EdDSA.
package jwt

import (
	"errors"
	"fmt"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidToken is returned for any malformed, tampered, or expired token.
// We intentionally do NOT distinguish between "expired" and "signature bad" in
// the public error so attackers can't probe which is which.
var ErrInvalidToken = errors.New("invalid token")

// Claims is the access-token claim set. We keep it small: the gateway only
// needs to know who the request is from. Authorisation decisions still happen
// in the handler, against the database, where ownership is the source of truth.
type Claims struct {
	UserID uuid.UUID `json:"sub"`
	gojwt.RegisteredClaims
}

// Service issues and parses access tokens. Build it once at app start.
type Service struct {
	secret    []byte
	accessTTL time.Duration
	issuer    string
}

// NewService constructs a JWT service with the given HS256 secret and access
// TTL. `issuer` is set in the `iss` claim; useful when multiple services share
// a key namespace.
func NewService(secret string, accessTTL time.Duration, issuer string) *Service {
	return &Service{
		secret:    []byte(secret),
		accessTTL: accessTTL,
		issuer:    issuer,
	}
}

// IssueAccess signs a token for the given user. The returned `expiresAt` is the
// absolute exp time, useful for the response payload so clients don't have to
// decode the JWT just to learn when it expires.
func (s *Service) IssueAccess(userID uuid.UUID) (token string, expiresAt time.Time, err error) {
	now := time.Now().UTC()
	expiresAt = now.Add(s.accessTTL)

	claims := Claims{
		UserID: userID,
		RegisteredClaims: gojwt.RegisteredClaims{
			Issuer:    s.issuer,
			IssuedAt:  gojwt.NewNumericDate(now),
			NotBefore: gojwt.NewNumericDate(now),
			ExpiresAt: gojwt.NewNumericDate(expiresAt),
			ID:        uuid.NewString(),
		},
	}
	t := gojwt.NewWithClaims(gojwt.SigningMethodHS256, claims)
	token, err = t.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign jwt: %w", err)
	}
	return token, expiresAt, nil
}

// Parse verifies a token's signature and expiry and returns its claims.
// Any failure is folded into ErrInvalidToken so callers can't accidentally
// leak why a token was rejected.
func (s *Service) Parse(token string) (*Claims, error) {
	parser := gojwt.NewParser(
		gojwt.WithValidMethods([]string{"HS256"}),
		gojwt.WithIssuedAt(),
	)
	claims := &Claims{}
	_, err := parser.ParseWithClaims(token, claims, func(t *gojwt.Token) (any, error) {
		return s.secret, nil
	})
	if err != nil {
		return nil, ErrInvalidToken
	}
	if claims.UserID == uuid.Nil {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
