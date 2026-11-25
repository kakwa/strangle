package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTokenCreateAndLookup(t *testing.T) {
	ctx := context.Background()
	ts := NewMemTokenStore()

	raw, tok, err := ts.Create(ctx, "admin", "ci", 0)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if !strings.HasPrefix(raw, TokenPrefix) || len(raw) != len(TokenPrefix)+2*TokenRandomSize {
		t.Errorf("raw token = %q, want %q prefix and 64 hex chars", raw, TokenPrefix)
	}
	if tok.Prefix != raw[:len(TokenPrefix)+8] {
		t.Errorf("Prefix = %q, want %q", tok.Prefix, raw[:len(TokenPrefix)+8])
	}
	if tok.Name != "ci" || tok.Expires != nil {
		t.Errorf("token = %+v", tok)
	}

	got, err := ts.Lookup(ctx, raw)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if got.Name != "ci" || got.Prefix != tok.Prefix {
		t.Errorf("Lookup() = %+v", got)
	}
	if got.LastUsedAt == nil {
		t.Error("Lookup() must record LastUsedAt")
	}

	if _, err := ts.Lookup(ctx, TokenPrefix+"0000000000000000000000000000000000000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup() unknown: err = %v, want ErrNotFound", err)
	}
}

func TestTokenNameUniquenessPerOwner(t *testing.T) {
	ctx := context.Background()
	ts := NewMemTokenStore()

	if _, _, err := ts.Create(ctx, "admin", "ci", 0); err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if _, _, err := ts.Create(ctx, "admin", "ci", 0); err == nil {
		t.Error("Create() with a duplicate name must fail")
	}
	// The same name is fine for another owner.
	if _, _, err := ts.Create(ctx, "other", "ci", 0); err != nil {
		t.Errorf("Create() for another owner: %v", err)
	}
	if _, _, err := ts.Create(ctx, "admin", "  ", 0); err == nil {
		t.Error("Create() with a blank name must fail")
	}
}

func TestTokenListAndRemove(t *testing.T) {
	ctx := context.Background()
	ts := NewMemTokenStore()

	if _, _, err := ts.Create(ctx, "admin", "ci", 0); err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if _, _, err := ts.Create(ctx, "admin", "deploy", 0); err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if _, _, err := ts.Create(ctx, "other", "ci", 0); err != nil {
		t.Fatalf("Create() error: %v", err)
	}

	list, err := ts.List(ctx, "admin")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(list) != 2 || list[0].Name != "ci" || list[1].Name != "deploy" {
		t.Errorf("List() = %+v, want ci then deploy", list)
	}

	if err := ts.Remove(ctx, "admin", "ci"); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	if err := ts.Remove(ctx, "admin", "ci"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Remove() twice: err = %v, want ErrNotFound", err)
	}
	// Removing "ci" must not touch the other owner's token.
	list, err = ts.List(ctx, "other")
	if err != nil || len(list) != 1 || list[0].Name != "ci" {
		t.Errorf("List(other) after admin remove = %+v, %v", list, err)
	}
}

func TestTokenExpiry(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	ts := &MemTokenStore{
		tokens: make(map[string]*memToken),
		names:  make(map[string]string),
		owners: make(map[string][]string),
		now:    func() time.Time { return now },
	}

	raw, tok, err := ts.Create(ctx, "admin", "short", time.Minute)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if tok.Expires == nil {
		t.Fatal("Expires must be set for a ttl token")
	}
	if _, err := ts.Lookup(ctx, raw); err != nil {
		t.Fatalf("Lookup() before expiry: %v", err)
	}

	now = now.Add(2 * time.Minute)
	if _, err := ts.Lookup(ctx, raw); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup() after expiry: err = %v, want ErrNotFound", err)
	}
	list, err := ts.List(ctx, "admin")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List() after expiry = %+v, want empty", list)
	}
}
