package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/utils/sshpool"
	"github.com/labstack/echo/v5"
)

// ListHosts godoc
//
// @ID      get_orgs_org_collections_collection_hosts
// @Summary Hosts of this collection
// @Tags    hosts
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Success 200 {array} model.HostRecord "Hosts"
// @Failure 401 {object} apiError "not authenticated"
// @Failure 403 {object} apiError "access denied"
// @Failure 404 {object} apiError "unknown collection"
// @Failure 500 {object} apiError "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hosts [get]
func (a *API) ListHosts(c *echo.Context) error {
	r := c.Request()
	coll, err := a.collectionScope(c)
	if err != nil {
		return err
	}
	hosts, err := a.Store.ListHosts(r.Context())
	if err != nil {
		return apiStatus(http.StatusInternalServerError, err)
	}
	allowed := map[string]bool{coll: true}
	if a.restricted(r.Context()) {
		allowed = a.accessibleTargets(r.Context(), r)
	}
	filtered := make([]model.HostRecord, 0, len(hosts))
	for _, h := range hosts {
		if h.Target == coll && targetVisible(allowed, h.Target) {
			filtered = append(filtered, h)
		}
	}
	return c.JSON(http.StatusOK, filtered)
}

// HostRuns godoc
//
// @ID      get_playbooks_playbook_hosts_host_runs
// @Summary Run history of one host for one playbook
// @Tags    hosts
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   playbook path string true "Playbook (job) name from strangle.yml"
// @Param   host path string true "Inventory host name"
// @Param   limit query int false "Maximum number of entries" default(10)
// @Success 200 {array} model.HostRunRecord "Host runs"
// @Failure 400 {object} apiError "invalid limit"
// @Failure 401 {object} apiError "not authenticated"
// @Failure 403 {object} apiError "access denied"
// @Failure 404 {object} apiError "unknown collection or playbook"
// @Failure 500 {object} apiError "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/runs [get]
func (a *API) HostRuns(c *echo.Context) error {
	r := c.Request()
	if _, err := a.playbookScope(c); err != nil {
		return err
	}
	coll, playbook := c.Param("collection"), c.Param("playbook")
	limit := intQuery(r, "limit", 10)
	// The store returns runs of this host across every collection, newest
	// first. Fetch them all, apply the route's scope and then the limit: a
	// global limit would hide matching runs behind newer runs of another
	// collection.
	runs, err := a.Store.HostRuns(r.Context(), c.Param("host"), 0)
	if err != nil {
		return apiStatus(http.StatusInternalServerError, err)
	}
	allowed := map[string]bool{coll: true}
	if a.restricted(r.Context()) {
		allowed = a.accessibleTargets(r.Context(), r)
	}
	filtered := make([]model.HostRunRecord, 0, len(runs))
	for _, run := range runs {
		if run.Target == coll && run.Playbook == playbook && targetVisible(allowed, run.Target) {
			filtered = append(filtered, run)
			if len(filtered) == limit {
				break
			}
		}
	}
	return c.JSON(http.StatusOK, filtered)
}

// HostKeys godoc
//
// @ID      get_playbooks_playbook_hosts_host_hostkeys
// @Summary Pending SSH host keys of one host for one playbook
// @Tags    ssh
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   playbook path string true "Playbook (job) name from strangle.yml"
// @Param   host path string true "Inventory host name"
// @Success 200 {array} sshpool.HostKey "Host keys"
// @Failure 401 {object} apiError "not authenticated"
// @Failure 403 {object} apiError "access denied"
// @Failure 404 {object} apiError "unknown collection or playbook"
// @Router  /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/hostkeys [get]
func (a *API) HostKeys(c *echo.Context) error {
	scope, err := a.playbookScope(c)
	if err != nil {
		return err
	}
	keys := []sshpool.HostKey{}
	if a.SSH != nil {
		for _, key := range a.SSH.PendingHostKeys() {
			if key.Target == scope && key.Host == c.Param("host") {
				keys = append(keys, key)
			}
		}
	}
	return c.JSON(http.StatusOK, keys)
}

// PatchHost godoc
//
// @ID      patch_playbooks_playbook_hosts_host
// @Summary Update a host's SSH key trust status with JSON Patch
// @Tags    ssh
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   playbook path string true "Playbook (job) name from strangle.yml"
// @Param   host path string true "Inventory host name"
// @Param   request body []patchOperation true "JSON Patch operations on /status and /fingerprint"
// @Success 200 {object} hostStatusResponse "Updated"
// @Failure 400 {object} apiError "invalid patch or status"
// @Failure 401 {object} apiError "not authenticated"
// @Failure 403 {object} apiError "access denied"
// @Failure 404 {object} apiError "no pending key or unknown collection/playbook"
// @Failure 409 {object} apiError "fingerprint mismatch"
// @Router  /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host} [patch]
func (a *API) PatchHost(c *echo.Context) error {
	scope, err := a.playbookScope(c)
	if err != nil {
		return err
	}
	var ops []patchOperation
	if err := c.Bind(&ops); err != nil {
		return apiStatus(http.StatusBadRequest, fmt.Errorf("invalid JSON Patch: %w", err))
	}
	var status, fingerprint string
	for _, op := range ops {
		var value *string
		switch op.Path {
		case "/status":
			value = &status
		case "/fingerprint":
			value = &fingerprint
		default:
			return apiStatus(http.StatusBadRequest, fmt.Errorf("unsupported JSON Patch path %q", op.Path))
		}
		if op.Op != "replace" && op.Op != "add" {
			return apiStatus(http.StatusBadRequest, fmt.Errorf("unsupported operation %q on %s", op.Op, op.Path))
		}
		*value = op.Value
	}
	if status != "approved" && status != "rejected" {
		return apiStatus(http.StatusBadRequest, errors.New("status must be \"approved\" or \"rejected\""))
	}
	if a.SSH == nil {
		return apiStatus(http.StatusBadRequest, errors.New("SSH host key approval is not enabled"))
	}
	host := c.Param("host")
	if status == "approved" {
		err = a.SSH.ApproveHostKey(scope, host, fingerprint)
	} else {
		err = a.SSH.RejectHostKey(scope, host)
	}
	if err != nil {
		switch {
		case errors.Is(err, sshpool.ErrNoPendingKey):
			return apiStatus(http.StatusNotFound, err)
		case errors.Is(err, sshpool.ErrFingerprintMismatch):
			return apiStatus(http.StatusConflict, err)
		default:
			return apiStatus(http.StatusBadRequest, err)
		}
	}
	return c.JSON(http.StatusOK, hostStatusResponse{Status: status, Host: host})
}
