// Package auth implements local UI authentication: argon2id password
// hashing, store interfaces backed by PostgreSQL and in-memory server-side
// sessions.
//
// Users are provisioned out of band (the `strangle-cli user` subcommand), not
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

	"golang.org/x/crypto/argon2"
)

// ErrNotFound is returned by user stores when a user does not exist.
var ErrNotFound = errors.New("auth: user not found")

// ErrInvalidMembership is returned when a collection grant lacks a user,
// org or collection name.
var ErrInvalidMembership = errors.New("auth: membership needs a user, org and collection")

// User is a local UI account. Hash is the argon2id PHC string; it is never
// serialized (JSON tags exclude it), so returning a User in an API response
// is safe.
type User struct {
	Name string `json:"name"`
	Hash string `json:"-"`
}

// UserStore stores local UI users. Implementations must be safe for
// concurrent use. The production implementation is db.UserStore on PostgreSQL.
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
	//nolint:gosec // G115: key is a decoded argon2 hash, far below uint32 range
	dk := argon2.Key([]byte(pw), salt, uint32(t), uint32(m), uint8(p), uint32(len(key)))
	return subtle.ConstantTimeCompare(dk, key) == 1
}
