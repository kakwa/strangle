package server

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/server/handlers"
	"github.com/labstack/echo/v5"
)

// authMiddleware resolves credentials once per request and rejects API
// requests which need a user when none was found. Handlers read the resolved
// identity through handlers.FromRequest.
func (s *Server) authMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		r := c.Request()
		a := s.resolveAuth(r)
		r = r.WithContext(handlers.WithAuth(r.Context(), a))
		c.SetRequest(r)
		c.Set("auth", a)

		if s.authOn && strings.HasPrefix(r.URL.Path, "/api/") &&
			a.User == "" && !s.authOpen(r) && !s.hookOK(r) {
			writeError(c.Response(), http.StatusUnauthorized, errors.New("not authenticated"))
			return nil
		}
		return next(c)
	}
}

// collectionAccessMiddleware applies the org/collection membership (and token
// scope) check to routes whose pattern carries both path parameters. Routes
// without a collection scope keep filtering per record in their handlers. It
// runs after authMiddleware, so MayUse sees the resolved identity.
func collectionAccessMiddleware(api *handlers.API) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			r := c.Request()
			org, collection := c.Param("org"), c.Param("collection")
			if org != "" && collection != "" && !api.MayUse(r.Context(), r, org, collection) {
				writeError(c.Response(), http.StatusForbidden, errors.New("access to this collection is denied"))
				return nil
			}
			return next(c)
		}
	}
}

// resolveAuth checks the credentials carried by r. A session wins over a
// bearer token; both are checked before the static token so a token endpoint
// never mistakes the machine credential for a user. Collection-scoped user
// tokens add a scope restriction to the resolved identity.
func (s *Server) resolveAuth(r *http.Request) handlers.Auth {
	var a handlers.Auth
	if sess, ok := s.sessionFrom(r); ok {
		a.User = sess.Username
		a.SessionUser = sess.Username
	} else if raw, ok := bearerTokenValue(r); ok && s.tokens != nil {
		if tok, err := s.tokens.Lookup(r.Context(), raw); err == nil {
			a.User = tok.Owner
			if tok.Org != "" || tok.Collection != "" {
				a.TokenScopes = []handlers.TokenScope{{Org: tok.Org, Collection: tok.Collection}}
			}
		}
	}
	a.StaticToken = s.bearerTokenOK(r)
	return a
}

// hookOK reports whether r may bypass the ordinary user requirement. The
// static sync token is only accepted on the collection hook route.
func (s *Server) hookOK(r *http.Request) bool {
	if !handlers.IsHookPath(r) {
		return false
	}
	a := handlers.FromRequest(r)
	return a.User != "" || a.StaticToken || (!s.authOn && s.authToken == "")
}

// authOpen lists the API endpoints reachable without a session.
func (s *Server) authOpen(r *http.Request) bool {
	// API docs are public but have no versioned alias beyond the spec itself.
	// Swagger UI assets live under /api/docs/ and must be public too.
	if r.Method == http.MethodGet && (r.URL.Path == "/api/docs" ||
		strings.HasPrefix(r.URL.Path, "/api/docs/") ||
		r.URL.Path == apiPrefix+"/openapi.json") {
		return true
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == apiPrefix+"/health":
		return true
	case r.Method == http.MethodPost && r.URL.Path == apiPrefix+"/login":
		return true
	case r.Method == http.MethodPost && r.URL.Path == apiPrefix+"/logout":
		return true
	case r.Method == http.MethodGet && r.URL.Path == apiPrefix+"/me":
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
