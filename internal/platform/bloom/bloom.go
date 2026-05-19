// Package bloom provides Redis Bloom-filter helpers for fast uniqueness
// checks before hitting Postgres.
//
// Flow (defence in depth):
//
//	1. BF.EXISTS → if "maybe seen", return 409 without bcrypt / INSERT.
//	2. INSERT with UNIQUE constraint → catches races and Bloom false negatives.
//	3. BF.ADD on successful INSERT (and on 23505) → keeps the filter warm.
//
// Requires Redis with the RedisBloom module (Redis Stack). See docker-compose.
package bloom

import "context"

// Filter is the contract for a single Bloom filter namespace (e.g. all emails).
type Filter interface {
	// MayContain reports whether the member might already be in the set.
	// true  → probably duplicate; reject early without touching Postgres.
	// false → definitely not in the filter; safe to proceed to INSERT.
	MayContain(ctx context.Context, member string) (bool, error)
	// Add records a member after a successful INSERT or unique violation.
	Add(ctx context.Context, member string) error
}
