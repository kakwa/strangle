package api

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/labstack/echo/v5"
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
// @Failure 401 {object} apiError "not authenticated"
// @Router  /api/v1/orgs [get]
func (a *API) ListOrgs(c *echo.Context) error {
	return c.JSON(http.StatusOK, a.visibleOrganizations(c.Request()))
}

// CreateOrg godoc
//
// @ID      post_orgs
// @Summary Create an organization
// @Tags    collections
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   request body orgCreateRequest true "organization"
// @Success 201 {object} orgCreateResponse "Created organization"
// @Failure 400 {object} apiError "invalid name"
// @Failure 401 {object} apiError "not authenticated"
// @Failure 409 {object} apiError "organization already exists"
// @Failure 503 {object} apiError "store unavailable"
// @Router  /api/v1/orgs [post]
func (a *API) CreateOrg(c *echo.Context) error {
	if len(FromRequest(c.Request()).TokenScopes) > 0 {
		return apiStatus(http.StatusForbidden, errors.New("a scoped token cannot create organizations"))
	}
	if a.Store == nil {
		return apiStatus(http.StatusServiceUnavailable, errors.New("organization storage is not configured"))
	}
	var req orgCreateRequest
	if err := c.Bind(&req); err != nil {
		return apiStatus(http.StatusBadRequest, err)
	}
	name := strings.TrimSpace(req.Name)
	if !orgNamePattern.MatchString(name) {
		return apiStatus(http.StatusBadRequest, errors.New("invalid organization name"))
	}
	org, err := a.Store.CreateOrg(c.Request().Context(), name)
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return apiStatus(http.StatusConflict, err)
		}
		return apiStatus(http.StatusInternalServerError, err)
	}
	return c.JSON(http.StatusCreated, orgCreateResponse{Name: org.Name, Collections: []collectionInfo{}})
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
// @Failure 401 {object} apiError "not authenticated"
// @Failure 404 {object} apiError "unknown org"
// @Router  /api/v1/orgs/{org} [get]
func (a *API) GetOrg(c *echo.Context) error {
	org := c.Param("org")
	for _, o := range a.visibleOrganizations(c.Request()) {
		if o.Name == org {
			return c.JSON(http.StatusOK, o)
		}
	}
	return apiStatus(http.StatusNotFound, fmt.Errorf("unknown org %q", org))
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
// @Failure 401 {object} apiError "not authenticated"
// @Failure 404 {object} apiError "unknown org"
// @Router  /api/v1/orgs/{org}/collections [get]
func (a *API) ListCollections(c *echo.Context) error {
	org := c.Param("org")
	for _, o := range a.visibleOrganizations(c.Request()) {
		if o.Name == org {
			return c.JSON(http.StatusOK, o.Collections)
		}
	}
	return apiStatus(http.StatusNotFound, fmt.Errorf("unknown org %q", org))
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
// @Success 200 {object} collectionDetailResponse "Collection"
// @Failure 401 {object} apiError "not authenticated"
// @Failure 403 {object} apiError "access denied"
// @Failure 404 {object} apiError "unknown collection"
// @Router  /api/v1/orgs/{org}/collections/{collection} [get]
func (a *API) GetCollection(c *echo.Context) error {
	t, err := a.requireCollection(c)
	if err != nil {
		return err
	}
	if !a.MayUse(c.Request().Context(), c.Request(), c.Param("org"), t.Name) {
		return apiStatus(http.StatusForbidden, errors.New("access to this collection is denied"))
	}
	return c.JSON(http.StatusOK, collectionDetailResponse{
		Name:      t.Name,
		Org:       orgOf(t),
		Playbooks: playbookInfos(t.Playbooks),
		Links:     collectionLinks(c.Param("org"), t.Name),
	})
}
