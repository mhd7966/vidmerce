package jwt

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/httpx"
)

// CtxKeyUserID is the Gin context key under which authenticated user IDs are
// stored. Handlers read this with c.MustGet(CtxKeyUserID).(uuid.UUID).
const CtxKeyUserID = "user_id"

// RequireAuth returns a Gin middleware that rejects any request lacking a
// valid bearer token. On success, it puts the parsed user ID into the request
// context under CtxKeyUserID.
//
// The middleware is intentionally strict: no fallbacks to cookies, query
// params, or any other transport. Bearer-token-only keeps the attack surface
// small and matches the spec.
func RequireAuth(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("Authorization")
		if raw == "" {
			httpx.Error(c, 401, httpx.CodeUnauthenticated, "missing authorization header")
			return
		}
		const prefix = "Bearer "
		if !strings.HasPrefix(raw, prefix) {
			httpx.Error(c, 401, httpx.CodeUnauthenticated, "malformed authorization header")
			return
		}
		token := strings.TrimSpace(raw[len(prefix):])
		if token == "" {
			httpx.Error(c, 401, httpx.CodeUnauthenticated, "empty bearer token")
			return
		}
		claims, err := svc.Parse(token)
		if err != nil {
			httpx.Error(c, 401, httpx.CodeUnauthenticated, "invalid or expired token")
			return
		}
		c.Set(CtxKeyUserID, claims.UserID)
		c.Next()
	}
}

// UserIDFrom returns the authenticated user's ID. Panics if RequireAuth has
// not run for this request — that's an inappropriate handler wiring and is
// always a bug, never a runtime condition we want to mask.
func UserIDFrom(c *gin.Context) uuid.UUID {
	return c.MustGet(CtxKeyUserID).(uuid.UUID)
}

// OptionalAuth is like RequireAuth but allows anonymous requests through. If
// the request carries a valid bearer token, UserID is populated in the gin
// context just like with RequireAuth. If the header is missing OR the token
// is malformed / expired, the request continues without UserID set.
//
// Used for endpoints that need to *attribute* a request to a user when
// possible (e.g. POST /videos/:id/view, where logged-in users get a per-user
// dedup window and anonymous viewers get an IP-based one) without forcing
// authentication.
func OptionalAuth(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("Authorization")
		if raw == "" {
			c.Next()
			return
		}
		const prefix = "Bearer "
		if !strings.HasPrefix(raw, prefix) {
			c.Next()
			return
		}
		token := strings.TrimSpace(raw[len(prefix):])
		if token == "" {
			c.Next()
			return
		}
		if claims, err := svc.Parse(token); err == nil {
			c.Set(CtxKeyUserID, claims.UserID)
		}
		c.Next()
	}
}

// OptionalUserIDFrom returns the user ID from context if OptionalAuth set it,
// or uuid.Nil with ok=false if the request was anonymous. Never panics.
func OptionalUserIDFrom(c *gin.Context) (uuid.UUID, bool) {
	v, exists := c.Get(CtxKeyUserID)
	if !exists {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}
