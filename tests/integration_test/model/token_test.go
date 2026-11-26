package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/security/auth"
	"github.com/kakwa/strangle/tests/testutil"
)

func TestTokenStoreCRUD(t *testing.T) {
	d, dsn := openTestDB(t)
	defer d.Close()

	ctx := context.Background()
	tokens := d.Tokens()
	const owner, org, collection, name = "admin", "acme", "home", "ci"

	raw, tok, err := tokens.Create(ctx, owner, org, collection, name, 0)
	if err != nil {
		t.Fatalf("Create() error: %v", err)
	}
	if !strings.HasPrefix(raw, auth.TokenPrefix) || len(raw) != len(auth.TokenPrefix)+2*auth.TokenRandomSize {
		t.Errorf("raw token = %q, want %q prefix and %d hex chars", raw, auth.TokenPrefix, 2*auth.TokenRandomSize)
	}
	if tok.Owner != owner || tok.Org != org || tok.Collection != collection || tok.Name != name ||
		tok.Prefix != raw[:len(auth.TokenPrefix)+8] {
		t.Errorf("token = %+v", tok)
	}

	if _, _, err := tokens.Create(ctx, owner, org, collection, name, 0); err == nil {
		t.Error("Create() on an existing name in the same scope must fail")
	}
	if _, _, err := tokens.Create(ctx, owner, org, collection, "  ", 0); err == nil {
		t.Error("Create() with a blank name must fail")
	}
	if _, _, err := tokens.Create(ctx, "other", org, collection, name, 0); err != nil {
		t.Errorf("Create() for another owner: %v", err)
	}

	got, err := tokens.Lookup(ctx, raw)
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if got.Name != name || got.Prefix != tok.Prefix || got.Org != org || got.Collection != collection {
		t.Errorf("Lookup() = %+v", got)
	}
	if got.LastUsedAt == nil {
		t.Error("Lookup() must record LastUsedAt")
	}
	if _, err := tokens.Lookup(ctx, auth.TokenPrefix+"0123456789012345678901234567890123456789"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Lookup() unknown: err = %v, want auth.ErrNotFound", err)
	}

	list, err := tokens.List(ctx, owner, org, collection)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(list) != 1 || list[0].Name != name || list[0].Org != org || list[0].Collection != collection {
		t.Errorf("List() = %+v", list)
	}
	// An empty scope filter disables that filter.
	if list, err := tokens.List(ctx, owner, "", ""); err != nil || len(list) != 1 {
		t.Errorf("List() without scope filters = %+v, %v", list, err)
	}

	// The raw value must not be stored anywhere in the table.
	var hash string
	if err := testutil.SQLDB(t, dsn).QueryRowContext(ctx,
		`SELECT token_hash FROM api_tokens
		 WHERE owner = $1 AND org = $2 AND collection = $3 AND name = $4`,
		owner, org, collection, name).Scan(&hash); err != nil {
		t.Fatalf("read record: %v", err)
	}
	if strings.Contains(hash, auth.TokenPrefix) || len(hash) != 64 {
		t.Errorf("stored token = %q, want a 64-char sha256 hex digest", hash)
	}

	// Removing with a different scope must not delete the token.
	if err := tokens.Remove(ctx, owner, org, "staging", name); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Remove() with wrong collection: err = %v, want auth.ErrNotFound", err)
	}
	if list, err := tokens.List(ctx, owner, org, collection); err != nil || len(list) != 1 {
		t.Errorf("List() after wrong-scope Remove() = %+v, %v; want the token kept", list, err)
	}

	if err := tokens.Remove(ctx, owner, org, collection, name); err != nil {
		t.Fatalf("Remove() error: %v", err)
	}
	if err := tokens.Remove(ctx, owner, org, collection, name); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Remove() twice: err = %v, want auth.ErrNotFound", err)
	}
}

func TestTokenStoreExpiry(t *testing.T) {
	d, dsn := openTestDB(t)
	defer d.Close()

	ctx := context.Background()
	tokens := d.Tokens()
	const owner, org, collection, name = "admin", "acme", "home", "short"

	raw, tok, err := tokens.Create(ctx, owner, org, collection, name, time.Minute)
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
		`UPDATE api_tokens SET expires_at = $1
		 WHERE owner = $2 AND org = $3 AND collection = $4 AND name = $5`,
		time.Now().Add(-time.Minute), owner, org, collection, name); err != nil {
		t.Fatalf("force expiry: %v", err)
	}
	if _, err := tokens.Lookup(ctx, raw); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Lookup() after expiry: err = %v, want auth.ErrNotFound", err)
	}
	list, err := tokens.List(ctx, owner, org, collection)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List() after expiry = %+v, want empty", list)
	}
}

// TestTokenStoreScoping checks that the org/collection pair is part of a
// token's identity: names may be reused across scopes, filters select only
// matching scopes, and removal requires the exact scope.
func TestTokenStoreScoping(t *testing.T) {
	d, _ := openTestDB(t)
	defer d.Close()

	ctx := context.Background()
	tokens := d.Tokens()
	const owner, name = "admin", "ci"
	scopes := []struct{ org, collection string }{
		{"acme", "home"},
		{"acme", "edge"},
		{"beta", "home"},
		{"", ""}, // legacy, unscoped token
	}
	raws := make(map[string]string, len(scopes))
	for _, sc := range scopes {
		raw, tok, err := tokens.Create(ctx, owner, sc.org, sc.collection, name, 0)
		if err != nil {
			t.Fatalf("Create(%q, %q) error: %v", sc.org, sc.collection, err)
		}
		if tok.Owner != owner || tok.Org != sc.org || tok.Collection != sc.collection || tok.Name != name {
			t.Errorf("Create(%q, %q) token = %+v", sc.org, sc.collection, tok)
		}
		b, err := json.Marshal(tok)
		if err != nil {
			t.Fatalf("json.Marshal() error: %v", err)
		}
		if !strings.Contains(string(b), `"org":"`+sc.org+`"`) ||
			!strings.Contains(string(b), `"collection":"`+sc.collection+`"`) {
			t.Errorf("token JSON = %s, want org and collection fields", b)
		}
		raws[sc.org+"/"+sc.collection] = raw
	}

	lookup, err := tokens.Lookup(ctx, raws["acme/home"])
	if err != nil {
		t.Fatalf("Lookup() error: %v", err)
	}
	if lookup.Org != "acme" || lookup.Collection != "home" {
		t.Errorf("Lookup() = %+v, want org=acme collection=home", lookup)
	}

	// List filters are combined; an empty org or collection disables just
	// that filter. Results are ordered by org, then collection, then name.
	listKeys := func(org, collection string) string {
		t.Helper()
		list, err := tokens.List(ctx, owner, org, collection)
		if err != nil {
			t.Fatalf("List(%q, %q) error: %v", org, collection, err)
		}
		keys := make([]string, 0, len(list))
		for _, tok := range list {
			keys = append(keys, tok.Org+"/"+tok.Collection+":"+tok.Name)
		}
		return strings.Join(keys, ",")
	}
	if got, want := listKeys("", ""), "/:ci,acme/edge:ci,acme/home:ci,beta/home:ci"; got != want {
		t.Errorf("List(all) = %q, want %q", got, want)
	}
	if got, want := listKeys("acme", ""), "acme/edge:ci,acme/home:ci"; got != want {
		t.Errorf("List(org=acme) = %q, want %q", got, want)
	}
	if got, want := listKeys("", "home"), "acme/home:ci,beta/home:ci"; got != want {
		t.Errorf("List(collection=home) = %q, want %q", got, want)
	}
	if got, want := listKeys("acme", "home"), "acme/home:ci"; got != want {
		t.Errorf("List(acme/home) = %q, want %q", got, want)
	}
	if got := listKeys("beta", "edge"); got != "" {
		t.Errorf("List(beta/edge) = %q, want empty", got)
	}
	if list, err := tokens.List(ctx, "other", "", ""); err != nil || len(list) != 0 {
		t.Errorf("List(other owner) = %+v, %v; want empty", list, err)
	}

	// Neither a different collection nor a different owner may remove it.
	if err := tokens.Remove(ctx, owner, "acme", "staging", name); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Remove() with wrong scope: err = %v, want auth.ErrNotFound", err)
	}
	if err := tokens.Remove(ctx, "other", "acme", "home", name); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Remove() with wrong owner: err = %v, want auth.ErrNotFound", err)
	}
	if got, want := listKeys("", ""), "/:ci,acme/edge:ci,acme/home:ci,beta/home:ci"; got != want {
		t.Errorf("List() after failed removals = %q, want %q", got, want)
	}

	if err := tokens.Remove(ctx, owner, "acme", "home", name); err != nil {
		t.Fatalf("Remove(acme/home) error: %v", err)
	}
	if got, want := listKeys("", ""), "/:ci,acme/edge:ci,beta/home:ci"; got != want {
		t.Errorf("List() after Remove() = %q, want %q", got, want)
	}
	if _, err := tokens.Lookup(ctx, raws["acme/home"]); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("Lookup() after Remove(): err = %v, want auth.ErrNotFound", err)
	}
}
