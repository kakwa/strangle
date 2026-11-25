package handlers

import (
	"errors"
	"net/http"

	"github.com/kakwa/strangle/internal/collection"
)

// IsHookPath reports whether r targets the collection hook route. The static
// machine token is accepted there and nowhere else.
func IsHookPath(r *http.Request) bool {
	return hookPath(r)
}

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
// @Param   request body syncRequestDoc true "hook payload"
// @Success 200 {object} syncResponseDoc "Synchronized"
// @Failure 400 {object} apiErrorDoc "invalid request or not a git collection"
// @Failure 401 {object} apiErrorDoc "missing or invalid credentials"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 502 {object} apiErrorDoc "git failure"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hook [post]
func (a *API) Hook(w http.ResponseWriter, r *http.Request) {
	if a.Syncer == nil {
		writeError(w, http.StatusNotFound, errors.New("no git-backed collection is configured"))
		return
	}
	if !a.hookAllowed(r) {
		writeError(w, http.StatusUnauthorized, errors.New("missing or invalid session or bearer token"))
		return
	}
	if _, ok := a.requireCollection(w, r); !ok {
		return
	}
	var req syncRequestDoc
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	coll := r.PathValue("collection")
	if !a.MayUse(r.Context(), r, r.PathValue("org"), coll) {
		writeError(w, http.StatusForbidden, errors.New("access to this collection is denied"))
		return
	}
	rev, changed, err := a.Syncer.Sync(r.Context(), coll, req.Branch)
	if err != nil {
		switch {
		case errors.Is(err, collection.ErrUnknownTarget):
			writeError(w, http.StatusNotFound, err)
		case errors.Is(err, collection.ErrNotGit):
			writeError(w, http.StatusBadRequest, err)
		default:
			writeError(w, http.StatusBadGateway, err)
		}
		return
	}
	if a.Applier != nil {
		a.Applier.UpdateTargets(a.Syncer.Targets())
	}
	writeJSON(w, http.StatusOK, syncResponseDoc{Target: coll, Rev: rev, Changed: changed})
}
