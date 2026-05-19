package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mhd7966/vidmerce/internal/platform/db"
)

// UserRepository abstracts the storage backend for users. Defined here (the
// consumer side) so the service can be unit-tested against an in-memory fake
// without touching pgx. The Postgres implementation lives in this same file
// to keep the package surface small.
type UserRepository interface {
	Create(ctx context.Context, email, passwordHash string) (User, error)
	FindByEmail(ctx context.Context, email string) (User, error)
	FindByID(ctx context.Context, id uuid.UUID) (User, error)
}

// pgUserRepo is the Postgres implementation of UserRepository.
type pgUserRepo struct {
	pool *db.Pool
}

// NewPostgresUserRepository constructs a repository bound to the given pgx pool.
func NewPostgresUserRepository(pool *db.Pool) UserRepository {
	return &pgUserRepo{pool: pool}
}

// pgUniqueViolation is Postgres's SQLSTATE for a unique-constraint conflict.
// We compare against this exact value so unrelated errors aren't swallowed.
const pgUniqueViolation = "23505"

func (r *pgUserRepo) Create(ctx context.Context, email, passwordHash string) (User, error) {
	const q = `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING id, email, password_hash, created_at
	`
	var u User
	err := r.pool.QueryRow(ctx, q, strings.ToLower(strings.TrimSpace(email)), passwordHash).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (r *pgUserRepo) FindByEmail(ctx context.Context, email string) (User, error) {
	const q = `
		SELECT id, email, password_hash, created_at
		FROM users
		WHERE email = $1
	`
	var u User
	err := r.pool.QueryRow(ctx, q, strings.ToLower(strings.TrimSpace(email))).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user by email: %w", err)
	}
	return u, nil
}

func (r *pgUserRepo) FindByID(ctx context.Context, id uuid.UUID) (User, error) {
	const q = `
		SELECT id, email, password_hash, created_at
		FROM users
		WHERE id = $1
	`
	var u User
	err := r.pool.QueryRow(ctx, q, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user by id: %w", err)
	}
	return u, nil
}

