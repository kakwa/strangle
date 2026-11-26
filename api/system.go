package api

import (
	"errors"
	"net/http"

	"github.com/kakwa/strangle/security/auth"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
)

// SessionCookie is the cookie carrying a UI session token. It lives here so
// handlers and the server's auth middleware share one definition.
const SessionCookie = "strangle_session"

// Health godoc
//
// @ID      get_health
// @Summary Liveness probe
// @Tags    system
// @Produce json
// @Success 200 {object} statusResponse "Server is up"
// @Router  /api/v1/health [get]
func (a *API) Health(c *echo.Context) error {
	return c.JSON(http.StatusOK, statusResponse{Status: "ok"})
}

// Login godoc
//
// @ID      post_login
// @Summary Log in and start a session
// @Tags    auth
// @Produce json
// @Param   request body loginRequest true "credentials"
// @Success 200 {object} meResponse "Authenticated; sets the strangle_session cookie"
// @Failure 400 {object} apiError "invalid JSON"
// @Failure 401 {object} apiError "invalid credentials"
// @Router  /api/v1/login [post]
func (a *API) Login(c *echo.Context) error {
	var req loginRequest
	if err := c.Bind(&req); err != nil {
		return apiStatus(http.StatusBadRequest, err)
	}
	if !a.AuthOn || a.Users == nil {
		return apiStatus(http.StatusUnauthorized,
			errors.New("authentication is not enabled: no users are configured (strangle-cli user add <name> --password <pw>)"))
	}
	user, err := a.Users.GetUser(c.Request().Context(), req.Username)
	if err != nil || !auth.VerifyPassword(user.Hash, req.Password) {
		// Same error for unknown user and wrong password: no account enumeration.
		return apiStatus(http.StatusUnauthorized, errors.New("invalid username or password"))
	}
	token := a.Sessions.Create(user.Name)
	c.SetCookie(&http.Cookie{
		Name:     SessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionTTL.Seconds()),
	})
	if a.Logger != nil {
		a.Logger.Info("user logged in", zap.String("username", user.Name))
	}
	return c.JSON(http.StatusOK, meResponse{Authenticated: true, Username: user.Name})
}

// Logout godoc
//
// @ID      post_logout
// @Summary Clear the session cookie
// @Tags    auth
// @Produce json
// @Success 200 {object} statusResponse "Logged out"
// @Router  /api/v1/logout [post]
func (a *API) Logout(c *echo.Context) error {
	if c, err := c.Request().Cookie(SessionCookie); err == nil && a.Sessions != nil {
		a.Sessions.Destroy(c.Value)
	}
	c.SetCookie(&http.Cookie{
		Name:   SessionCookie,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	return c.JSON(http.StatusOK, statusResponse{Status: "logged out"})
}

// Me godoc
//
// @ID      get_me
// @Summary Current session identity
// @Tags    auth
// @Produce json
// @Success 200 {object} meResponse "Identity (or unauthenticated)"
// @Failure 401 {object} apiError "not authenticated"
// @Router  /api/v1/me [get]
func (a *API) Me(c *echo.Context) error {
	if !a.AuthOn {
		return c.JSON(http.StatusOK, meResponse{})
	}
	username, ok := SessionUserFrom(c.Request())
	if !ok {
		return apiStatus(http.StatusUnauthorized, errors.New("not authenticated"))
	}
	return c.JSON(http.StatusOK, meResponse{Authenticated: true, Username: username})
}

// OpenAPISpec godoc
//
// @ID      get_openapi_json
// @Summary OpenAPI 3 description of the versioned API
// @Tags    system
// @Produce json
// @Success 200 {object} map[string]any "OpenAPI document"
// @Router  /api/v1/openapi.json [get]
//
// The document itself is served by the server package; this anchor keeps the
// annotation beside the handler group that owns the versioned API.
func (a *API) OpenAPISpec() {}

// SwaggerUI godoc
//
// @ID      get_api_docs
// @Summary Swagger UI for the versioned API
// @Tags    system
// @Produce html
// @Success 200 {string} string "Swagger UI"
// @Router  /api/docs [get]
func (a *API) SwaggerUI() {}
