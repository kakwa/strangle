package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
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
// use and must never store or return the raw token value.
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

// MemTokenStore is an in-memory TokenStore, used in tests; production uses
// the gorm-backed store in package db.
type MemTokenStore struct {
	mu     sync.RWMutex
	now    func() time.Time
	tokens map[string]*memToken // token key -> record
	names  map[string]string    // "owner\x00name" -> key (uniqueness)
	owners map[string][]string  // owner -> keys (listing)
}

type memToken struct {
	Token
	key string
}

// NewMemTokenStore returns an empty in-memory token store.
func NewMemTokenStore() *MemTokenStore {
	return &MemTokenStore{
		now:    time.Now,
		tokens: make(map[string]*memToken),
		names:  make(map[string]string),
		owners: make(map[string][]string),
	}
}

func ownerNameKey(owner, name string) string { return owner + "\x00" + name }

func (m *MemTokenStore) Create(_ context.Context, owner, name string, ttl time.Duration) (string, *Token, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, errors.New("auth: token name is empty")
	}
	raw, err := NewTokenValue()
	if err != nil {
		return "", nil, err
	}
	key := TokenKey(raw)
	now := m.now()
	var expires *time.Time
	if ttl > 0 {
		t := now.Add(ttl)
		expires = &t
	}
	rec := &memToken{
		Token: Token{Owner: owner, Name: name, Prefix: raw[:len(TokenPrefix)+8], CreatedAt: now, Expires: expires},
		key:   key,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.names[ownerNameKey(owner, name)]; ok {
		return "", nil, fmt.Errorf("auth: token %q already exists", name)
	}
	m.tokens[key] = rec
	m.names[ownerNameKey(owner, name)] = key
	m.owners[owner] = append(m.owners[owner], key)
	return raw, cloneToken(&rec.Token), nil
}

func (m *MemTokenStore) Lookup(_ context.Context, raw string) (*Token, error) {
	key := TokenKey(raw)
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.tokens[key]
	if !ok {
		return nil, ErrNotFound
	}
	if rec.Expires != nil && m.now().After(*rec.Expires) {
		m.removeLocked(rec)
		return nil, ErrNotFound
	}
	now := m.now()
	rec.LastUsedAt = &now
	return cloneToken(&rec.Token), nil
}

func (m *MemTokenStore) List(_ context.Context, owner string) ([]*Token, error) {
	m.mu.RLock()
	keys := append([]string(nil), m.owners[owner]...)
	recs := make([]*memToken, 0, len(keys))
	for _, k := range keys {
		if rec, ok := m.tokens[k]; ok {
			recs = append(recs, rec)
		}
	}
	m.mu.RUnlock()
	// Drop expired entries from the listing (Lookup prunes them for real).
	now := m.now()
	out := make([]*Token, 0, len(recs))
	for _, r := range recs {
		if r.Expires != nil && now.After(*r.Expires) {
			continue
		}
		out = append(out, cloneToken(&r.Token))
	}
	return out, nil
}

func (m *MemTokenStore) Remove(_ context.Context, owner, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, ok := m.names[ownerNameKey(owner, name)]
	if !ok {
		return ErrNotFound
	}
	rec, ok := m.tokens[key]
	if !ok {
		delete(m.names, ownerNameKey(owner, name))
		return ErrNotFound
	}
	m.removeLocked(rec)
	return nil
}

// removeLocked drops rec from all indexes. The caller holds the lock.
func (m *MemTokenStore) removeLocked(rec *memToken) {
	delete(m.tokens, rec.key)
	delete(m.names, ownerNameKey(rec.Owner, rec.Name))
	keys := m.owners[rec.Owner]
	for i, k := range keys {
		if k == rec.key {
			m.owners[rec.Owner] = append(keys[:i], keys[i+1:]...)
			break
		}
	}
}

func (m *MemTokenStore) Close() error { return nil }

func cloneToken(t *Token) *Token {
	c := *t
	if t.Expires != nil {
		e := *t.Expires
		c.Expires = &e
	}
	if t.LastUsedAt != nil {
		l := *t.LastUsedAt
		c.LastUsedAt = &l
	}
	return &c
}
