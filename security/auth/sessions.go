package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// SessionTTL is how long a UI session stays valid after it is created (and
// after every request, since tokens are re-issued on login only).
const SessionTTL = 12 * time.Hour

// Session is one authenticated UI login.
type Session struct {
	Username string
	Expires  time.Time
}

// Sessions is an in-memory session table for the lifetime of the master
// process. Sessions do not survive a restart.
type Sessions struct {
	mu    sync.RWMutex
	items map[string]*Session
	now   func() time.Time
	ttl   time.Duration
}

// NewSessions returns an empty session table with the default 12h TTL.
func NewSessions() *Sessions {
	return &Sessions{
		items: make(map[string]*Session),
		now:   time.Now,
		ttl:   SessionTTL,
	}
}

// Create stores a new session for username and returns its random token.
func (s *Sessions) Create(username string) string {
	token := randomToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[token] = &Session{Username: username, Expires: s.now().Add(s.ttl)}
	s.pruneLocked()
	return token
}

// Lookup returns the session for token, or nil if the token is unknown or
// expired. Expired entries are removed on lookup.
func (s *Sessions) Lookup(token string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.items[token]
	if !ok {
		return nil
	}
	if s.now().After(sess.Expires) {
		delete(s.items, token)
		return nil
	}
	return sess
}

// Destroy removes token. Unknown tokens are ignored.
func (s *Sessions) Destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, token)
}

// Len returns the number of live (non-expired) sessions.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return len(s.items)
}

func (s *Sessions) pruneLocked() {
	now := s.now()
	for tok, sess := range s.items {
		if now.After(sess.Expires) {
			delete(s.items, tok)
		}
	}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("auth: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b)
}
