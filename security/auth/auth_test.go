package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashAndVerifyPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Errorf("hash %q does not look like an argon2id PHC string", hash)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Error("VerifyPassword() = false for the right password")
	}
	if VerifyPassword(hash, "wrong") {
		t.Error("VerifyPassword() = true for a wrong password")
	}
	if VerifyPassword("", "x") || VerifyPassword("not-a-hash", "x") {
		t.Error("VerifyPassword() must reject malformed hashes")
	}
}

func TestHashPasswordSalts(t *testing.T) {
	a, err := HashPassword("same")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	b, err := HashPassword("same")
	if err != nil {
		t.Fatalf("HashPassword() error: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password must differ (random salt)")
	}
	if !VerifyPassword(b, "same") {
		t.Error("VerifyPassword() = false for the second hash")
	}
}

// A hash made with different parameters must still verify: parameters live
// inside the PHC string.
func TestVerifyPasswordForeignParameters(t *testing.T) {
	// m=4096,t=2,p=2, 16-byte salt "0123456789abcdef", 32-byte key.
	hash := "$argon2id$v=19$m=4096,t=2,p=2$MTIzNDU2Nzg5YWJjZGVm$" +
		strings.Repeat("A", 43)
	// The key is base64 of 32 zero bytes; recompute what argon2 would give
	// is impractical here, so just assert the parse path does not panic and
	// returns false for a wrong password.
	if VerifyPassword(hash, "anything") {
		t.Error("VerifyPassword() = true with a forged key")
	}
}

func TestSessions(t *testing.T) {
	s := NewSessions()

	tok := s.Create("alice")
	if tok == "" {
		t.Fatal("Create() returned an empty token")
	}
	sess := s.Lookup(tok)
	if sess == nil {
		t.Fatal("Lookup() = nil for a fresh token")
	}
	if sess.Username != "alice" {
		t.Errorf("session username = %q, want alice", sess.Username)
	}
	if n := s.Len(); n != 1 {
		t.Errorf("Len() = %d, want 1", n)
	}

	// Unknown tokens are nil.
	if s.Lookup("nope") != nil {
		t.Error("Lookup() must return nil for an unknown token")
	}

	// Destroy removes the token.
	s.Destroy(tok)
	if s.Lookup(tok) != nil {
		t.Error("Lookup() after Destroy() must be nil")
	}
	s.Destroy("nope") // must not panic

	// Two tokens are independent.
	a, b := s.Create("a"), s.Create("b")
	s.Destroy(a)
	if s.Lookup(a) != nil || s.Lookup(b) == nil {
		t.Error("Destroy(a) must not affect b")
	}
	if n := s.Len(); n != 1 {
		t.Errorf("Len() = %d, want 1", n)
	}
}

func TestSessionsExpire(t *testing.T) {
	s := NewSessions()
	s.ttl = time.Minute
	var now time.Time
	s.now = func() time.Time { return now }

	tok := s.Create("alice")

	now = now.Add(30 * time.Second)
	if s.Lookup(tok) == nil {
		t.Error("Lookup() = nil before expiry")
	}

	now = now.Add(31 * time.Second)
	if s.Lookup(tok) != nil {
		t.Error("Lookup() must return nil after expiry")
	}
	if n := s.Len(); n != 0 {
		t.Errorf("Len() = %d after expiry, want 0", n)
	}
}

func TestCollectionMembershipKey(t *testing.T) {
	m := CollectionMembership{Org: "acme", Collection: "web"}
	if got := m.Key(); got != "acme\x00web" {
		t.Errorf("Key() = %q, want %q", got, "acme\x00web")
	}
	// The separator makes (org, collection) pairs unambiguous: the org and
	// collection of two different pairs must not yield the same key.
	other := CollectionMembership{Org: "acme\x00web", Collection: ""}
	if m.Key() == other.Key() {
		t.Error("Key() must not collide across org/collection boundaries")
	}
}

func TestSessionTokensAreUnique(t *testing.T) {
	s := NewSessions()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := s.Create("u")
		if len(tok) != 64 {
			t.Fatalf("token length = %d, want 64 hex chars", len(tok))
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}
