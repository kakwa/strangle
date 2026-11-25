package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// orgNamePattern keeps API-created org names usable as URL segments and in
// configuration files.
var orgNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ListOrgs godoc
//
// @ID      get_orgs
// @Summary List visible organizations and their collections
// @Tags    collections
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Success 200 {array} orgInfo "Organizations"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Router  /api/v1/orgs [get]
func (a *API) ListOrgs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.visibleOrganizations(r))
}

// CreateOrg godoc
//
// @ID      post_orgs
// @Summary Create an organization
// @Tags    collections
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   request body orgCreateRequestDoc true "organization"
// @Success 201 {object} orgCreateResponseDoc "Created organization"
// @Failure 400 {object} apiErrorDoc "invalid name"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 409 {object} apiErrorDoc "organization already exists"
// @Failure 503 {object} apiErrorDoc "store unavailable"
// @Router  /api/v1/orgs [post]
func (a *API) CreateOrg(w http.ResponseWriter, r *http.Request) {
	if len(FromRequest(r).TokenScopes) > 0 {
		writeError(w, http.StatusForbidden, errors.New("a scoped token cannot create organizations"))
		return
	}
	if a.Store == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("organization storage is not configured"))
		return
	}
	var req orgCreateRequestDoc
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if !orgNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, errors.New("invalid organization name"))
		return
	}
	org, err := a.Store.CreateOrg(r.Context(), name)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, orgCreateResponseDoc{Name: org.Name, Collections: []collectionInfo{}})
}

// GetOrg godoc
//
// @ID      get_orgs_org
// @Summary Describe one organization
// @Tags    collections
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Success 200 {object} orgInfo "Organization"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 404 {object} apiErrorDoc "unknown org"
// @Router  /api/v1/orgs/{org} [get]
func (a *API) GetOrg(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	for _, o := range a.visibleOrganizations(r) {
		if o.Name == org {
			writeJSON(w, http.StatusOK, o)
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("unknown org %q", org))
}

// ListCollections godoc
//
// @ID      get_orgs_org_collections
// @Summary List the collections of an organization
// @Tags    collections
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Success 200 {array} collectionInfo "Collections"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 404 {object} apiErrorDoc "unknown org"
// @Router  /api/v1/orgs/{org}/collections [get]
func (a *API) ListCollections(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	for _, o := range a.visibleOrganizations(r) {
		if o.Name == org {
			writeJSON(w, http.StatusOK, o.Collections)
			return
		}
	}
	writeError(w, http.StatusNotFound, fmt.Errorf("unknown org %q", org))
}

// GetCollection godoc
//
// @ID      get_orgs_org_collections_collection
// @Summary Describe one collection and its sub-resource links
// @Tags    collections
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Success 200 {object} collectionDetailResponseDoc "Collection"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Router  /api/v1/orgs/{org}/collections/{collection} [get]
func (a *API) GetCollection(w http.ResponseWriter, r *http.Request) {
	t, ok := a.requireCollection(w, r)
	if !ok {
		return
	}
	if !a.MayUse(r.Context(), r, r.PathValue("org"), t.Name) {
		writeError(w, http.StatusForbidden, errors.New("access to this collection is denied"))
		return
	}
	writeJSON(w, http.StatusOK, collectionDetailResponseDoc{
		Name:      t.Name,
		Org:       orgOf(t),
		Playbooks: playbookInfos(t.Playbooks),
		Links:     collectionLinks(r.PathValue("org"), t.Name),
	})
}
