package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/kakwa/strangle/security/auth"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
)

// tokenScope resolves and validates the org/collection of a token route.
// Tokens are managed only through a collection URL.
func (a *API) tokenScope(c *echo.Context) (string, string, error) {
	if _, err := a.requireCollection(c); err != nil {
		return "", "", err
	}
	return c.Param("org"), c.Param("collection"), nil
}

// tokenOwner resolves the user who may manage tokens of the collection, or a
// 403 status error when the request carries no user identity.
func (a *API) tokenOwner(r *http.Request) (string, error) {
	owner, ok := UserFrom(r)
	if !ok {
		return "", apiStatus(http.StatusForbidden, errors.New("a user account is required to manage tokens (use a session or a user token)"))
	}
	return owner, nil
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
// @Failure 401 {object} apiError "not authenticated"
// @Failure 403 {object} apiError "a user account is required"
// @Failure 404 {object} apiError "unknown collection"
// @Failure 503 {object} apiError "token storage is not configured"
// @Router  /api/v1/orgs/{org}/collections/{collection}/tokens [get]
func (a *API) ListTokens(c *echo.Context) error {
	if a.Tokens == nil {
		return apiStatus(http.StatusServiceUnavailable, errors.New("token storage is not configured"))
	}
	r := c.Request()
	org, collection, err := a.tokenScope(c)
	if err != nil {
		return err
	}
	owner, err := a.tokenOwner(r)
	if err != nil {
		return err
	}
	tokens, err := a.Tokens.List(r.Context(), owner, org, collection)
	if err != nil {
		return apiStatus(http.StatusInternalServerError, err)
	}
	return c.JSON(http.StatusOK, tokens)
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
// @Param   request body tokenCreateRequest true "token details"
// @Success 201 {object} tokenCreateResponse "Created; raw value is shown once"
// @Failure 400 {object} apiError "invalid request"
// @Failure 401 {object} apiError "not authenticated"
// @Failure 403 {object} apiError "a user account is required"
// @Failure 404 {object} apiError "unknown collection"
// @Failure 409 {object} apiError "token already exists"
// @Failure 503 {object} apiError "token storage is not configured"
// @Router  /api/v1/orgs/{org}/collections/{collection}/tokens [post]
func (a *API) CreateToken(c *echo.Context) error {
	if a.Tokens == nil {
		return apiStatus(http.StatusServiceUnavailable, errors.New("token storage is not configured"))
	}
	r := c.Request()
	org, collection, err := a.tokenScope(c)
	if err != nil {
		return err
	}
	owner, err := a.tokenOwner(r)
	if err != nil {
		return err
	}
	var req tokenCreateRequest
	if err := c.Bind(&req); err != nil {
		return apiStatus(http.StatusBadRequest, err)
	}
	raw, tok, err := a.Tokens.Create(r.Context(), owner, org, collection, req.Name, durationHours(req.TTLHours))
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return apiStatus(http.StatusConflict, err)
		}
		return apiStatus(http.StatusBadRequest, err)
	}
	if a.Logger != nil {
		a.Logger.Info("API token created", zap.String("username", owner), zap.String("token", tok.Name),
			zap.String("org", org), zap.String("collection", collection))
	}
	return c.JSON(http.StatusCreated, tokenCreateResponse{Raw: raw, Token: *tok})
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
// @Failure 401 {object} apiError "not authenticated"
// @Failure 403 {object} apiError "a user account is required"
// @Failure 404 {object} apiError "unknown collection or token"
// @Failure 503 {object} apiError "token storage is not configured"
// @Router  /api/v1/orgs/{org}/collections/{collection}/tokens/{name} [delete]
func (a *API) DeleteToken(c *echo.Context) error {
	if a.Tokens == nil {
		return apiStatus(http.StatusServiceUnavailable, errors.New("token storage is not configured"))
	}
	r := c.Request()
	org, collection, err := a.tokenScope(c)
	if err != nil {
		return err
	}
	owner, err := a.tokenOwner(r)
	if err != nil {
		return err
	}
	if err := a.Tokens.Remove(r.Context(), owner, org, collection, c.Param("name")); err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			return apiStatus(http.StatusNotFound, err)
		}
		return apiStatus(http.StatusInternalServerError, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func durationHours(hours int64) time.Duration {
	return time.Duration(hours) * time.Hour
}
