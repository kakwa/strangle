package handlers

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/auth"
	"go.uber.org/zap"
)

// tokenScopeFromRequest resolves and validates the org/collection of a token
// route. Tokens are managed only through a collection URL.
func (a *API) tokenScopeFromRequest(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	if _, ok := a.requireCollection(w, r); !ok {
		return "", "", false
	}
	return r.PathValue("org"), r.PathValue("collection"), true
}

func (a *API) tokenOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	owner, ok := UserFrom(r)
	if !ok {
		writeError(w, http.StatusForbidden, errors.New("a user account is required to manage tokens (use a session or a user token)"))
		return "", false
	}
	return owner, true
}

// ListTokens godoc
//
// @ID      get_orgs_org_collections_collection_tokens
// @Summary List collection-scoped personal access tokens
// @Tags    tokens
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Success 200 {array} auth.Token "Tokens"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "a user account is required"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 503 {object} apiErrorDoc "token storage is not configured"
// @Router  /api/v1/orgs/{org}/collections/{collection}/tokens [get]
func (a *API) ListTokens(w http.ResponseWriter, r *http.Request) {
	if a.Tokens == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("token storage is not configured"))
		return
	}
	org, collection, ok := a.tokenScopeFromRequest(w, r)
	if !ok {
		return
	}
	owner, ok := a.tokenOwner(w, r)
	if !ok {
		return
	}
	tokens, err := a.Tokens.List(r.Context(), owner, org, collection)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tokens)
}

// CreateToken godoc
//
// @ID      post_orgs_org_collections_collection_tokens
// @Summary Create a collection-scoped personal access token
// @Tags    tokens
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   request body tokenCreateRequestDoc true "token details"
// @Success 201 {object} tokenCreateResponseDoc "Created; raw value is shown once"
// @Failure 400 {object} apiErrorDoc "invalid request"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "a user account is required"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 409 {object} apiErrorDoc "token already exists"
// @Failure 503 {object} apiErrorDoc "token storage is not configured"
// @Router  /api/v1/orgs/{org}/collections/{collection}/tokens [post]
func (a *API) CreateToken(w http.ResponseWriter, r *http.Request) {
	if a.Tokens == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("token storage is not configured"))
		return
	}
	org, collection, ok := a.tokenScopeFromRequest(w, r)
	if !ok {
		return
	}
	owner, ok := a.tokenOwner(w, r)
	if !ok {
		return
	}
	var req tokenCreateRequestDoc
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	raw, tok, err := a.Tokens.Create(r.Context(), owner, org, collection, req.Name, durationHours(req.TTLHours))
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if a.Logger != nil {
		a.Logger.Info("API token created", zap.String("username", owner), zap.String("token", tok.Name),
			zap.String("org", org), zap.String("collection", collection))
	}
	writeJSON(w, http.StatusCreated, tokenCreateResponseDoc{Raw: raw, Token: *tok})
}

// DeleteToken godoc
//
// @ID      delete_orgs_org_collections_collection_tokens_name
// @Summary Revoke a collection-scoped personal access token
// @Tags    tokens
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   name path string true "Token name"
// @Success 204 {string} string "Revoked"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "a user account is required"
// @Failure 404 {object} apiErrorDoc "unknown collection or token"
// @Failure 503 {object} apiErrorDoc "token storage is not configured"
// @Router  /api/v1/orgs/{org}/collections/{collection}/tokens/{name} [delete]
func (a *API) DeleteToken(w http.ResponseWriter, r *http.Request) {
	if a.Tokens == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("token storage is not configured"))
		return
	}
	org, collection, ok := a.tokenScopeFromRequest(w, r)
	if !ok {
		return
	}
	owner, ok := a.tokenOwner(w, r)
	if !ok {
		return
	}
	if err := a.Tokens.Remove(r.Context(), owner, org, collection, r.PathValue("name")); err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func durationHours(hours int64) time.Duration {
	return time.Duration(hours) * time.Hour
}
