// Package auth's collection access layer maps users to the collections of
// the org/collection model. For now membership is explicit; groups and
// collection ownership can be layered on top of the same interface later.
package auth

import (
	"context"
	"sort"
	"strings"
	"sync"
)

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
// authentication does not accidentally lock the UI down.
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

// collectionAccessKey joins an org and collection name.
func collectionAccessKey(org, collection string) string {
	return org + "\x00" + collection
}

// MemCollectionAccess is an in-memory CollectionAccess used in tests and
// when no database is configured.
type MemCollectionAccess struct {
	mu   sync.RWMutex
	byID map[string]map[string]struct{} // username -> set of collection keys
}

// NewMemCollectionAccess returns an empty in-memory access store.
func NewMemCollectionAccess() *MemCollectionAccess {
	return &MemCollectionAccess{byID: map[string]map[string]struct{}{}}
}

func (m *MemCollectionAccess) Add(_ context.Context, username, org, collection string) error {
	if username == "" || org == "" || collection == "" {
		return ErrInvalidMembership
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byID[username] == nil {
		m.byID[username] = map[string]struct{}{}
	}
	m.byID[username][collectionAccessKey(org, collection)] = struct{}{}
	return nil
}

func (m *MemCollectionAccess) Remove(_ context.Context, username, org, collection string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if users, ok := m.byID[username]; ok {
		delete(users, collectionAccessKey(org, collection))
		if len(users) == 0 {
			delete(m.byID, username)
		}
	}
	return nil
}

func (m *MemCollectionAccess) List(_ context.Context, username string) ([]CollectionMembership, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []CollectionMembership{}
	for user, keys := range m.byID {
		if username != "" && user != username {
			continue
		}
		for key := range keys {
			org, collection, _ := strings.Cut(key, "\x00")
			out = append(out, CollectionMembership{Username: user, Org: org, Collection: collection})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Username != out[j].Username {
			return out[i].Username < out[j].Username
		}
		if out[i].Org != out[j].Org {
			return out[i].Org < out[j].Org
		}
		return out[i].Collection < out[j].Collection
	})
	return out, nil
}

func (m *MemCollectionAccess) HasAny(context.Context) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID) > 0, nil
}

func (m *MemCollectionAccess) Allows(_ context.Context, username, org, collection string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.byID[username][collectionAccessKey(org, collection)]
	return ok, nil
}

func (m *MemCollectionAccess) Close() error { return nil }
