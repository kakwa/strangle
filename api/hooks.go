package api

import (
	"errors"
	"net/http"

	"github.com/kakwa/strangle/controller/collection"
	"github.com/labstack/echo/v5"
)

// hookAllowed reports whether the resolved identity may call the hook.
func (a *API) hookAllowed(r *http.Request) bool {
	identity := FromRequest(r)
	return identity.User != "" || identity.StaticToken || (!a.AuthOn && a.AuthToken == "")
}

// Hook godoc
//
// @ID      post_orgs_org_collections_collection_hook
// @Summary Synchronize a git-backed collection (git hook)
// @Tags    collections
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Security syncToken
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   request body syncRequest true "hook payload"
// @Success 200 {object} syncResponse "Synchronized"
// @Failure 400 {object} apiError "invalid request or not a git collection"
// @Failure 401 {object} apiError "missing or invalid credentials"
// @Failure 403 {object} apiError "access denied"
// @Failure 404 {object} apiError "unknown collection"
// @Failure 502 {object} apiError "git failure"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hook [post]
func (a *API) Hook(c *echo.Context) error {
	r := c.Request()
	if a.Syncer == nil {
		return apiStatus(http.StatusNotFound, errors.New("no git-backed collection is configured"))
	}
	if !a.hookAllowed(r) {
		return apiStatus(http.StatusUnauthorized, errors.New("missing or invalid session or bearer token"))
	}
	if _, err := a.requireCollection(c); err != nil {
		return err
	}
	var req syncRequest
	if err := c.Bind(&req); err != nil {
		return apiStatus(http.StatusBadRequest, err)
	}
	coll := c.Param("collection")
	if !a.MayUse(r.Context(), r, c.Param("org"), coll) {
		return apiStatus(http.StatusForbidden, errors.New("access to this collection is denied"))
	}
	rev, changed, err := a.Syncer.Sync(r.Context(), coll, req.Branch)
	if err != nil {
		switch {
		case errors.Is(err, collection.ErrUnknownTarget):
			return apiStatus(http.StatusNotFound, err)
		case errors.Is(err, collection.ErrNotGit):
			return apiStatus(http.StatusBadRequest, err)
		default:
			return apiStatus(http.StatusBadGateway, err)
		}
	}
	if a.Applier != nil {
		a.Applier.UpdateTargets(a.Syncer.Targets())
	}
	return c.JSON(http.StatusOK, syncResponse{Target: coll, Rev: rev, Changed: changed})
}
