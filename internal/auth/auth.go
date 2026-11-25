// Package auth implements local UI authentication: argon2id password
// hashing, a user store, and in-memory server-side sessions.
//
// Users are provisioned out of band (the `strangle user` subcommand), not
// through the API. Sessions live in the master process only, so they do not
// survive a restart: the UI asks for a login again afterwards.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// ErrNotFound is returned by user stores when a user does not exist.
var ErrNotFound = errors.New("auth: user not found")

// User is a local UI account. Hash is the argon2id PHC string; it is never
// serialized (JSON tags exclude it), so returning a User in an API response
// is safe.
type User struct {
	Name string `json:"name"`
	Hash string `json:"-"`
}

// UserStore stores local UI users. Implementations must be safe for
// concurrent use.
type UserStore interface {
	// CreateUser stores a new user with the given argon2id hash. It fails
	// if the name already exists.
	CreateUser(ctx context.Context, name, hash string) error
	// GetUser returns the user with the given name, or ErrNotFound.
	GetUser(ctx context.Context, name string) (*User, error)
	// ListUsers returns all users, sorted by name.
	ListUsers(ctx context.Context) ([]*User, error)
	// Count returns the number of users.
	Count(ctx context.Context) (int, error)
	// RemoveUser deletes the user with the given name, or ErrNotFound.
	RemoveUser(ctx context.Context, name string) error
	Close() error
}

// argon2id parameters for new hashes. They are embedded in the PHC string,
// so VerifyPassword works with hashes made under other parameters.
const (
	argonTime    uint32 = 1
	argonMemory  uint32 = 64 * 1024
	argonThreads uint8  = 4
	argonKeyLen  uint32 = 32
	argonSaltLen int    = 16
)

// HashPassword produces an argon2id PHC string ($argon2id$v=19$m=...,t=...,
// p=...$salt$key) for pw.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	dk := argon2.Key([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf(
		"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk),
	), nil
}

// VerifyPassword reports whether pw matches the PHC string. It is constant-
// time and accepts any argon2id hash, regardless of the parameters used to
// create it.
func VerifyPassword(hash, pw string) bool {
	parts := strings.SplitN(hash, "$", 6)
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint64
	var p uint64
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	dk := argon2.Key([]byte(pw), salt, uint32(t), uint32(m), uint8(p), uint32(len(key)))
	return subtle.ConstantTimeCompare(dk, key) == 1
}

// MemUserStore is an in-memory UserStore. It is used in tests; production
// uses the gorm-backed store in package db.
type MemUserStore struct {
	mu    sync.RWMutex
	users map[string]string // name -> hash
}

// NewMemUserStore returns an empty in-memory user store.
func NewMemUserStore() *MemUserStore {
	return &MemUserStore{users: make(map[string]string)}
}

func (m *MemUserStore) CreateUser(_ context.Context, name, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[name]; ok {
		return fmt.Errorf("auth: user %q already exists", name)
	}
	m.users[name] = hash
	return nil
}

func (m *MemUserStore) GetUser(_ context.Context, name string) (*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	hash, ok := m.users[name]
	if !ok {
		return nil, ErrNotFound
	}
	return &User{Name: name, Hash: hash}, nil
}

func (m *MemUserStore) ListUsers(_ context.Context) ([]*User, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*User, 0, len(m.users))
	for name, hash := range m.users {
		out = append(out, &User{Name: name, Hash: hash})
	}
	// Map iteration order is random; sort for stable output.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

func (m *MemUserStore) Count(_ context.Context) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users), nil
}

func (m *MemUserStore) RemoveUser(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.users[name]; !ok {
		return ErrNotFound
	}
	delete(m.users, name)
	return nil
}

func (m *MemUserStore) Close() error { return nil }
