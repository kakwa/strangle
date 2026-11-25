package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/testutil"
)

func TestTokenStoreCRUD(t *testing.T) {
	d, dsn := openTestDB(t)
	defer d.Close()

	ctx := context.Background()
	tokens := d.Tokens()

	raw, tok, err := tokens.Create(ctx, "admin", "ci", 0)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if !strings.HasPrefix(raw, auth.TokenPrefix) {
		t.Errorf("raw token = %q, want %q prefix", raw, auth.TokenPrefix)
	}
	if tok.Name != "ci" || tok.Prefix != raw[:len(auth.TokenPrefix)+8] {
		t.Errorf("token = %+v", tok)
	}

	if _, _, err := tokens.Create(ctx, "admin", "ci", 0); err == nil {
		t.Error("Create() on an existing name must fail")
	}
	if _, _, err := tokens.Create(ctx, "admin", "  ", 0); err == nil {
		t.Error("Create() with a blank name must fail")
	}
	if _, _, err := tokens.Create(ctx, "other", "ci", 0); err != nil {
		t.Errorf("Create() for another owner: %v", err)
	}

	got, err := tokens.Lookup(ctx, raw)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if got.Name != "ci" || got.Prefix != tok.Prefix {
		t.Errorf("Lookup() = %+v", got)
	}
	if got.LastUsedAt == nil {
		t.Error("Lookup() must record LastUsedAt")
	}
	if _, err := tokens.Lookup(ctx, auth.TokenPrefix+"0123456789012345678901234567890123456789"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Lookup() unknown: err = %v, want auth.ErrNotFound", err)
	}

	list, err := tokens.List(ctx, "admin")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(list) != 1 || list[0].Name != "ci" {
		t.Errorf("List() = %+v", list)
	}

	// The raw value must not be stored anywhere in the table.
	var hash string
	if err := testutil.SQLDB(t, dsn).QueryRowContext(ctx,
		`SELECT token_hash FROM api_tokens WHERE name = $1`, "ci").Scan(&hash); err != nil {
		t.Fatalf("read record: %v", err)
	}
	if strings.Contains(hash, auth.TokenPrefix) || len(hash) != 64 {
		t.Errorf("stored token = %q, want a 64-char sha256 hex digest", hash)
	}

	if err := tokens.Remove(ctx, "admin", "ci"); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	if err := tokens.Remove(ctx, "admin", "ci"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Remove() twice: err = %v, want auth.ErrNotFound", err)
	}
}

func TestTokenStoreExpiry(t *testing.T) {
	d, dsn := openTestDB(t)
	defer d.Close()

	ctx := context.Background()
	tokens := d.Tokens()

	raw, tok, err := tokens.Create(ctx, "admin", "short", time.Minute)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if tok.Expires == nil {
		t.Fatal("Expires must be set for a ttl token")
	}
	if _, err := tokens.Lookup(ctx, raw); err != nil {
		t.Fatalf("Lookup() before expiry: %v", err)
	}

	// Force the expiry into the past.
	if _, err := testutil.SQLDB(t, dsn).ExecContext(ctx,
		`UPDATE api_tokens SET expires_at = $1 WHERE name = $2`,
		time.Now().Add(-time.Minute), "short"); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	if _, err := tokens.Lookup(ctx, raw); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Lookup() after expiry: err = %v, want auth.ErrNotFound", err)
	}
	list, err := tokens.List(ctx, "admin")
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List() after expiry = %+v, want empty", list)
	}
}
