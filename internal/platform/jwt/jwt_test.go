package jwt_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mhd7966/vidmerce/internal/platform/jwt"
)

func newSvc(t *testing.T, ttl time.Duration) *jwt.Service {
	t.Helper()
	return jwt.NewService("test-secret-must-be-long-enough", ttl, "test")
}

func TestRoundTripValidToken(t *testing.T) {
	svc := newSvc(t, time.Minute)
	uid := uuid.New()

	tok, exp, err := svc.IssueAccess(uid)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if tok == "" {
		t.Fatal("empty token")
	}
	if time.Until(exp) <= 0 {
		t.Fatalf("token already expired: %v", exp)
	}

	claims, err := svc.Parse(tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.UserID != uid {
		t.Fatalf("user id mismatch: got %v want %v", claims.UserID, uid)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	svc := newSvc(t, -time.Second) // already expired the moment it's signed
	uid := uuid.New()

	tok, _, err := svc.IssueAccess(uid)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := svc.Parse(tok); err == nil {
		t.Fatal("expected parse to reject expired token")
	}
}

func TestWrongSecretRejected(t *testing.T) {
	signer := jwt.NewService("secret-A", time.Minute, "test")
	verifier := jwt.NewService("secret-B", time.Minute, "test")

	tok, _, err := signer.IssueAccess(uuid.New())
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if _, err := verifier.Parse(tok); err == nil {
		t.Fatal("expected parse to reject mismatched-signature token")
	}
}

func TestGarbageTokenRejected(t *testing.T) {
	svc := newSvc(t, time.Minute)
	cases := []string{
		"",
		"not-a-jwt",
		"a.b.c",
		strings.Repeat("x", 1024),
	}
	for _, c := range cases {
		if _, err := svc.Parse(c); err == nil {
			t.Errorf("expected parse to reject %q", c)
		}
	}
}
