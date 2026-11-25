package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/sshpool"
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
// @Success 200 {array} db.HostRecord "Hosts"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hosts [get]
func (a *API) ListHosts(w http.ResponseWriter, r *http.Request) {
	coll, ok := a.collectionScope(r)
	if !ok {
		writeError(w, http.StatusNotFound, unknownCollection(r.PathValue("org"), r.PathValue("collection")))
		return
	}
	hosts, err := a.Store.ListHosts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	allowed := map[string]bool{coll: true}
	if a.restricted(r.Context()) {
		allowed = a.accessibleTargets(r.Context(), r)
	}
	filtered := make([]db.HostRecord, 0, len(hosts))
	for _, h := range hosts {
		if h.Target == coll && targetVisible(allowed, h.Target) {
			filtered = append(filtered, h)
		}
	}
	writeJSON(w, http.StatusOK, filtered)
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
// @Success 200 {array} db.HostRunRecord "Host runs"
// @Failure 400 {object} apiErrorDoc "invalid limit"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection or playbook"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/runs [get]
func (a *API) HostRuns(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.playbookScope(w, r); !ok {
		return
	}
	coll, playbook := r.PathValue("collection"), r.PathValue("playbook")
	limit := intQuery(r, "limit", 10)
	// The store returns runs of this host across every collection, newest
	// first. Fetch them all, apply the route's scope and then the limit: a
	// global limit would hide matching runs behind newer runs of another
	// collection.
	runs, err := a.Store.HostRuns(r.Context(), r.PathValue("host"), 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	allowed := map[string]bool{coll: true}
	if a.restricted(r.Context()) {
		allowed = a.accessibleTargets(r.Context(), r)
	}
	filtered := make([]db.HostRunRecord, 0, len(runs))
	for _, run := range runs {
		if run.Target == coll && run.Playbook == playbook && targetVisible(allowed, run.Target) {
			filtered = append(filtered, run)
			if len(filtered) == limit {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, filtered)
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
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection or playbook"
// @Router  /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}/hostkeys [get]
func (a *API) HostKeys(w http.ResponseWriter, r *http.Request) {
	scope, ok := a.playbookScope(w, r)
	if !ok {
		return
	}
	keys := []sshpool.HostKey{}
	if a.SSH != nil {
		for _, key := range a.SSH.PendingHostKeys() {
			if key.Target == scope && key.Host == r.PathValue("host") {
				keys = append(keys, key)
			}
		}
	}
	writeJSON(w, http.StatusOK, keys)
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
// @Param   request body []jsonPatchOperationDoc true "JSON Patch operations on /status and /fingerprint"
// @Success 200 {object} hostStatusResponseDoc "Updated"
// @Failure 400 {object} apiErrorDoc "invalid patch or status"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "no pending key or unknown collection/playbook"
// @Failure 409 {object} apiErrorDoc "fingerprint mismatch"
// @Router  /api/v1/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host} [patch]
func (a *API) PatchHost(w http.ResponseWriter, r *http.Request) {
	scope, ok := a.playbookScope(w, r)
	if !ok {
		return
	}
	var ops []jsonPatchOperationDoc
	if err := decodeJSON(r, &ops); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON Patch: %w", err))
		return
	}
	var status, fingerprint string
	for _, op := range ops {
		switch op.Path {
		case "/status":
			if op.Op != "replace" && op.Op != "add" {
				writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported operation %q on /status", op.Op))
				return
			}
			status = op.Value
		case "/fingerprint":
			if op.Op != "replace" && op.Op != "add" {
				writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported operation %q on /fingerprint", op.Op))
				return
			}
			fingerprint = op.Value
		default:
			writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported JSON Patch path %q", op.Path))
			return
		}
	}
	if status != "approved" && status != "rejected" {
		writeError(w, http.StatusBadRequest, errors.New("status must be \"approved\" or \"rejected\""))
		return
	}
	if a.SSH == nil {
		writeError(w, http.StatusBadRequest, errors.New("SSH host key approval is not enabled"))
		return
	}
	host := r.PathValue("host")
	var err error
	if status == "approved" {
		err = a.SSH.ApproveHostKey(scope, host, fingerprint)
	} else {
		err = a.SSH.RejectHostKey(scope, host)
	}
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "no pending host key"):
			writeError(w, http.StatusNotFound, err)
		case strings.Contains(err.Error(), "fingerprint mismatch"):
			writeError(w, http.StatusConflict, err)
		default:
			writeError(w, http.StatusBadRequest, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, hostStatusResponseDoc{Status: status, Host: host})
}
