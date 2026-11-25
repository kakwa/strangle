package handlers

import (
	"errors"
	"net/http"

	"github.com/kakwa/strangle/internal/auth"
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
// @Success 200 {object} messageResponseDoc "Server is up"
// @Router  /api/v1/health [get]
func (a *API) Health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, messageResponseDoc{Status: "ok"})
}

// Login godoc
//
// @ID      post_login
// @Summary Log in and start a session
// @Tags    auth
// @Produce json
// @Param   request body loginRequestDoc true "credentials"
// @Success 200 {object} meResponseDoc "Authenticated; sets the strangle_session cookie"
// @Failure 400 {object} apiErrorDoc "invalid JSON"
// @Failure 401 {object} apiErrorDoc "invalid credentials"
// @Router  /api/v1/login [post]
func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequestDoc
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !a.AuthOn || a.Users == nil {
		writeError(w, http.StatusUnauthorized,
			errors.New("authentication is not enabled: no users are configured (strangle-cli user add <name> --password <pw>)"))
		return
	}
	user, err := a.Users.GetUser(r.Context(), req.Username)
	if err != nil || !auth.VerifyPassword(user.Hash, req.Password) {
		// Same error for unknown user and wrong password: no account enumeration.
		writeError(w, http.StatusUnauthorized, errors.New("invalid username or password"))
		return
	}
	token := a.Sessions.Create(user.Name)
	http.SetCookie(w, &http.Cookie{
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
	writeJSON(w, http.StatusOK, meResponse{Authenticated: true, Username: user.Name})
}

// Logout godoc
//
// @ID      post_logout
// @Summary Clear the session cookie
// @Tags    auth
// @Produce json
// @Success 200 {object} messageResponseDoc "Logged out"
// @Router  /api/v1/logout [post]
func (a *API) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(SessionCookie); err == nil && a.Sessions != nil {
		a.Sessions.Destroy(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   SessionCookie,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, messageResponseDoc{Status: "logged out"})
}

// Me godoc
//
// @ID      get_me
// @Summary Current session identity
// @Tags    auth
// @Produce json
// @Success 200 {object} meResponseDoc "Identity (or unauthenticated)"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Router  /api/v1/me [get]
func (a *API) Me(w http.ResponseWriter, r *http.Request) {
	if !a.AuthOn {
		writeJSON(w, http.StatusOK, meResponse{})
		return
	}
	username, ok := SessionUserFrom(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("not authenticated"))
		return
	}
	writeJSON(w, http.StatusOK, meResponse{Authenticated: true, Username: username})
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
