package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/kakwa/strangle/internal/auth"
	"github.com/labstack/echo/v5"
)

// requestAuth is the identity resolved for one request by authMiddleware.
// A user is either a UI session or a user-owned API token; the static sync
// token is a machine credential and deliberately is not a user.
type requestAuth struct {
	user        string
	sessionUser string
	staticToken bool
}

type requestAuthKey struct{}

func withRequestAuth(ctx context.Context, a requestAuth) context.Context {
	return context.WithValue(ctx, requestAuthKey{}, a)
}

// authFromRequest returns the identity resolved by authMiddleware. It returns
// a zero value when the middleware has not run (for example in a unit test
// calling a handler directly).
func authFromRequest(r *http.Request) requestAuth {
	if a, ok := r.Context().Value(requestAuthKey{}).(requestAuth); ok {
		return a
	}
	return requestAuth{}
}

func userFromRequest(r *http.Request) (string, bool) {
	a := authFromRequest(r)
	return a.user, a.user != ""
}

func sessionUserFromRequest(r *http.Request) (string, bool) {
	a := authFromRequest(r)
	return a.sessionUser, a.sessionUser != ""
}

// authMiddleware is the single place where incoming credentials are checked.
// It resolves the request's identity once, stores it on the Echo context and
// on the request for wrapped http handlers, then rejects API requests which
// need a user when none was found.
func (s *Server) authMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		r := c.Request()
		a := s.resolveAuth(r)
		r = r.WithContext(withRequestAuth(r.Context(), a))
		c.SetRequest(r)
		c.Set("auth", a)

		if s.authOn && strings.HasPrefix(r.URL.Path, "/api/") &&
			a.user == "" && !s.authOpen(r) && !s.syncOK(r) {
			writeError(c.Response(), http.StatusUnauthorized, errors.New("not authenticated"))
			return nil
		}
		return next(c)
	}
}

// resolveAuth checks the credentials carried by r. A session wins over a
// bearer token; both are checked before the static token so that a token
// endpoint never mistakes the machine credential for a user.
func (s *Server) resolveAuth(r *http.Request) requestAuth {
	var a requestAuth
	if sess, ok := s.sessionFrom(r); ok {
		a.user = sess.Username
		a.sessionUser = sess.Username
	} else if raw, ok := bearerTokenValue(r); ok && s.tokens != nil {
		if tok, err := s.tokens.Lookup(r.Context(), raw); err == nil {
			a.user = tok.Owner
		}
	}
	a.staticToken = s.bearerTokenOK(r)
	return a
}

// syncOK reports whether r may use POST /api/sync: any valid identity works,
// the static sync token works on that route only, and when auth is off with
// no static token configured the endpoint is open.
func (s *Server) syncOK(r *http.Request) bool {
	path := apiAliasPath(r.URL.Path)
	if r.Method != http.MethodPost || (path != "/api/sync" && !strings.HasSuffix(path, "/sync")) {
		return false
	}
	return s.syncAllowed(r)
}

// syncAllowed reports whether the resolved identity may use the sync route.
func (s *Server) syncAllowed(r *http.Request) bool {
	a := authFromRequest(r)
	return a.user != "" || a.staticToken || (!s.authOn && s.authToken == "")
}

// authOpen lists the API endpoints reachable without a session, under both
// the versioned prefix and their flat aliases.
func (s *Server) authOpen(r *http.Request) bool {
	// API docs are public but have no legacy flat alias for the spec. The
	// Swagger UI assets live under /api/docs/ and must be public too.
	if r.Method == http.MethodGet && (r.URL.Path == "/api/docs" ||
		strings.HasPrefix(r.URL.Path, "/api/docs/") ||
		r.URL.Path == apiPrefix+"/openapi.json") {
		return true
	}
	path := apiAliasPath(r.URL.Path)
	switch {
	case path == "/api/health" && r.Method == http.MethodGet:
		return true
	case path == "/api/login" && r.Method == http.MethodPost:
		return true
	case path == "/api/logout" && r.Method == http.MethodPost:
		return true
	case path == "/api/me" && r.Method == http.MethodGet:
		return true
	}
	return false
}

// sessionFrom resolves the session carried by the request cookie, if any.
func (s *Server) sessionFrom(r *http.Request) (*auth.Session, bool) {
	if s.sessions == nil {
		return nil, false
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil, false
	}
	sess := s.sessions.Lookup(c.Value)
	if sess == nil {
		return nil, false
	}
	return sess, true
}

// bearerTokenValue reads a bearer credential from the Authorization header,
// falling back to the webhook header only when no Authorization header is
// present.
func bearerTokenValue(r *http.Request) (string, bool) {
	value := r.Header.Get("Authorization")
	if token := r.Header.Get("X-Strangle-Token"); token != "" && value == "" {
		value = "Bearer " + token
	}
	token, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	return token, true
}

// bearerTokenOK reports whether r carries the static configured API token.
func (s *Server) bearerTokenOK(r *http.Request) bool {
	if s.authToken == "" {
		return false
	}
	token, ok := bearerTokenValue(r)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) == 1
}
