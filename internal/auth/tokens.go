package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Token is one API credential owned by a user, for user-linked CLIs,
// webhooks and other integrations. The raw value is returned exactly once,
// at creation; afterwards only its hash is known.
type Token struct {
	Owner      string     `json:"-"` // the user the token belongs to
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"` // first chars of the raw value, for display
	CreatedAt  time.Time  `json:"created_at"`
	Expires    *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// TokenStore stores API tokens. Implementations must be safe for concurrent
// use and must never store or return the raw token value. The production
// implementation is db.TokenStore on PostgreSQL.
type TokenStore interface {
	// Create generates a new token for owner under the given name and
	// returns the raw value (shown once) and the stored record. ttl <= 0
	// means the token never expires. It fails if owner already has a
	// token with that name.
	Create(ctx context.Context, owner, name string, ttl time.Duration) (string, *Token, error)
	// Lookup validates a raw token (from an Authorization header, say) and
	// returns its record, or ErrNotFound. It records the use.
	Lookup(ctx context.Context, raw string) (*Token, error)
	// List returns owner's tokens, sorted by name.
	List(ctx context.Context, owner string) ([]*Token, error)
	// Remove deletes owner's token with the given name, or ErrNotFound.
	Remove(ctx context.Context, owner, name string) error
	Close() error
}

// TokenPrefix marks a raw token; TokenRandomSize is how many bytes of
// randomness a raw token carries.
const (
	TokenPrefix     = "st_"
	TokenRandomSize = 32
)

// TokenKey is the stored form of a raw token: its SHA-256 hex digest. Stores
// keep only the key, never the raw value.
func TokenKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// NewTokenValue returns a fresh raw token.
func NewTokenValue() (string, error) {
	b := make([]byte, TokenRandomSize)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return TokenPrefix + hex.EncodeToString(b), nil
}
