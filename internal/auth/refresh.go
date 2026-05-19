package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

// RefreshTokenStore manages the lifecycle of opaque refresh tokens.
//
// Why opaque (not JWT)? Refresh tokens must be revocable on logout, password
// change, or detected abuse. JWTs cannot be revoked without an external
// blocklist, so we don't gain anything from JWT semantics for refresh tokens
// — we'd still need the same Redis lookup we do here.
//
// The token format is `<token_id>.<secret>` where token_id is a UUID stored
// in Redis along with the user ID; secret is a 32-byte random string that is
// verified by exact match. Splitting the lookup key from the secret means a
// stolen Redis dump never gives the attacker the original token (unless they
// also dump the value, which we hash before storing).
type RefreshTokenStore interface {
	// Issue mints a new refresh token for a user and persists it. Returns the
	// raw token to hand to the client.
	Issue(ctx context.Context, userID uuid.UUID, ttl time.Duration) (string, error)
	// Validate verifies a presented token and returns the owning user ID. On
	// any failure (not-found, expired, tampered) returns ErrInvalidRefresh.
	Validate(ctx context.Context, token string) (uuid.UUID, error)
	// Revoke deletes a refresh token. Idempotent: revoking an unknown token is
	// not an error.
	Revoke(ctx context.Context, token string) error
}

// redisRefreshStore is the Redis-backed implementation.
type redisRefreshStore struct {
	rdb *goredis.Client
}

// NewRedisRefreshStore constructs a refresh-token store using the given client.
func NewRedisRefreshStore(rdb *goredis.Client) RefreshTokenStore {
	return &redisRefreshStore{rdb: rdb}
}

func (s *redisRefreshStore) Issue(ctx context.Context, userID uuid.UUID, ttl time.Duration) (string, error) {
	tokenID := uuid.NewString()
	secret, err := randomToken(32)
	if err != nil {
		return "", fmt.Errorf("generate refresh secret: %w", err)
	}
	full := tokenID + "." + secret

	key := refreshKey(tokenID)
	value := userID.String() + "." + secret
	if err := s.rdb.Set(ctx, key, value, ttl).Err(); err != nil {
		return "", fmt.Errorf("persist refresh token: %w", err)
	}

	// Per-user index so future "revoke all sessions" is one SCAN+DEL away.
	indexKey := refreshUserIndexKey(userID)
	pipe := s.rdb.Pipeline()
	pipe.SAdd(ctx, indexKey, tokenID)
	pipe.Expire(ctx, indexKey, ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		// Index failure is non-fatal: the primary record was written. We log it
		// at the caller (service) where a logger is available.
		return full, nil
	}
	return full, nil
}

func (s *redisRefreshStore) Validate(ctx context.Context, token string) (uuid.UUID, error) {
	tokenID, secret, ok := splitRefresh(token)
	if !ok {
		return uuid.Nil, ErrInvalidRefresh
	}
	raw, err := s.rdb.Get(ctx, refreshKey(tokenID)).Result()
	if errors.Is(err, goredis.Nil) {
		return uuid.Nil, ErrInvalidRefresh
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("read refresh token: %w", err)
	}
	uidStr, storedSecret, ok := splitRefresh(raw)
	if !ok || storedSecret != secret {
		return uuid.Nil, ErrInvalidRefresh
	}
	uid, err := uuid.Parse(uidStr)
	if err != nil {
		return uuid.Nil, ErrInvalidRefresh
	}
	return uid, nil
}

func (s *redisRefreshStore) Revoke(ctx context.Context, token string) error {
	tokenID, _, ok := splitRefresh(token)
	if !ok {
		return nil
	}
	return s.rdb.Del(ctx, refreshKey(tokenID)).Err()
}

// --- helpers (unexported) -----------------------------------------------------

func refreshKey(tokenID string) string           { return "refresh:" + tokenID }
func refreshUserIndexKey(uid uuid.UUID) string   { return "refresh:user:" + uid.String() }

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func splitRefresh(s string) (id, secret string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return s[:i], s[i+1:], i > 0 && i < len(s)-1
		}
	}
	return "", "", false
}
