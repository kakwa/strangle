// Package auth's collection access layer maps users to the collections of
// the org/collection model. For now membership is explicit; groups and
// collection ownership can be layered on top of the same interface later.
package auth

import "context"

// CollectionMembership grants one user access to one collection of one org.
type CollectionMembership struct {
	Username   string `json:"username"`
	Org        string `json:"org"`
	Collection string `json:"collection"`
}

// Key identifies a collection independently of its string representation.
func (m CollectionMembership) Key() string {
	return m.Org + "\x00" + m.Collection
}

// CollectionAccess stores user-to-collection memberships. HasAny tells the
// API whether restrictions are configured at all: while no membership exists
// every authenticated user keeps access to every collection, so enabling UI
// authentication does not accidentally lock the UI down. The production
// implementation is db.CollectionAccess on PostgreSQL.
type CollectionAccess interface {
	// Add grants access. Adding an existing membership is a no-op.
	Add(ctx context.Context, username, org, collection string) error
	// Remove revokes access. Removing a missing membership is a no-op.
	Remove(ctx context.Context, username, org, collection string) error
	// List returns the memberships, optionally restricted to one user
	// (empty username means all users).
	List(ctx context.Context, username string) ([]CollectionMembership, error)
	// HasAny reports whether at least one membership is configured.
	HasAny(ctx context.Context) (bool, error)
	// Allows reports whether username may use org/collection.
	Allows(ctx context.Context, username, org, collection string) (bool, error)
	// Close releases underlying resources.
	Close() error
}
