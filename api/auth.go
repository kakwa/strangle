package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

// authMiddleware resolves credentials once per request and rejects API
// requests which need a user when none was found. Handlers read the resolved
// identity through FromRequest.
func (s *Server) authMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		r := c.Request()
		a := s.resolveAuth(r)
		r = r.WithContext(WithAuth(r.Context(), a))
		c.SetRequest(r)

		if s.authOn && strings.HasPrefix(r.URL.Path, "/api/") &&
			a.User == "" && !s.authOpen(r) && !s.hookOK(r) {
			return apiStatus(http.StatusUnauthorized, errors.New("not authenticated"))
		}
		return next(c)
	}
}

// collectionAccessMiddleware applies the org/collection membership (and token
// scope) check to routes whose pattern carries both path parameters. Routes
// without a collection scope keep filtering per record in their handler. It
// runs after authMiddleware, so MayUse sees the resolved identity.
func collectionAccessMiddleware(api *API) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			r := c.Request()
			org, collection := c.Param("org"), c.Param("collection")
			if org != "" && collection != "" && !api.MayUse(r.Context(), r, org, collection) {
				return apiStatus(http.StatusForbidden, errors.New("access to this collection is denied"))
			}
			return next(c)
		}
	}
}
