package server

import (
	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/sshpool"
	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/internal/worker"
)

// Keep the packages referenced by the annotations in this file's imports so
// swag can resolve the type names while walking the AST.
var (
	_ auth.Token
	_ sshpool.HostKey
	_ store.Stats
	_ worker.WorkerStatus
)

// This file contains the swaggo/swag annotations for the canonical
// /api/v1 routes. Legacy flat /api/* aliases are intentionally not
// annotated: the generated document describes the versioned API only.
//
// The functions are documentation anchors: swag reads their comments, but
// they are never called. Keeping the annotations in one place makes it easy
// to regenerate the document with `make swag`.

// docHealth godoc
//
// @ID      get_health
// @Summary Liveness probe
// @Tags    system
// @Produce json
// @Success 200 {object} messageResponseDoc "Server is up"
// @Router  /api/v1/health [get]
func (s *Server) docHealth() {}

// docLogin godoc
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
func (s *Server) docLogin() {}

// docLogout godoc
//
// @ID      post_logout
// @Summary Clear the session cookie
// @Tags    auth
// @Produce json
// @Success 200 {object} messageResponseDoc "Logged out"
// @Router  /api/v1/logout [post]
func (s *Server) docLogout() {}

// docMe godoc
//
// @ID      get_me
// @Summary Current session identity
// @Tags    auth
// @Produce json
// @Success 200 {object} meResponseDoc "Identity (or unauthenticated)"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Router  /api/v1/me [get]
func (s *Server) docMe() {}

// docListTokens godoc
//
// @ID      get_tokens
// @Summary List personal access tokens
// @Tags    auth
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Success 200 {array} auth.Token "Tokens"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "a user account is required"
// @Failure 503 {object} apiErrorDoc "token storage is not configured"
// @Router  /api/v1/tokens [get]
func (s *Server) docListTokens() {}

// docCreateToken godoc
//
// @ID      post_tokens
// @Summary Create a personal access token
// @Tags    auth
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   request body tokenCreateRequestDoc true "token details"
// @Success 201 {object} tokenCreateResponseDoc "Created; raw value is shown once"
// @Failure 400 {object} apiErrorDoc "invalid request"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "a user account is required"
// @Failure 409 {object} apiErrorDoc "token already exists"
// @Failure 503 {object} apiErrorDoc "token storage is not configured"
// @Router  /api/v1/tokens [post]
func (s *Server) docCreateToken() {}

// docDeleteToken godoc
//
// @ID      delete_tokens_name
// @Summary Revoke a personal access token
// @Tags    auth
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   name path string true "Token name"
// @Success 204 {string} string "Revoked"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "a user account is required"
// @Failure 404 {object} apiErrorDoc "token not found"
// @Failure 500 {object} apiErrorDoc "store error"
// @Failure 503 {object} apiErrorDoc "token storage is not configured"
// @Router  /api/v1/tokens/{name} [delete]
func (s *Server) docDeleteToken() {}

// docStats godoc
//
// @ID      get_stats
// @Summary Aggregate statistics for visible collections
// @Tags    stats
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Success 200 {object} store.Stats "Statistics"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/stats [get]
func (s *Server) docStats() {}

// docCollectionStats godoc
//
// @ID      get_orgs_org_collections_collection_stats
// @Summary Statistics for this collection
// @Tags    stats
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Success 200 {object} store.Stats "Statistics"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/stats [get]
func (s *Server) docCollectionStats() {}

// docRuns godoc
//
// @ID      get_runs
// @Summary List runs without captured output
// @Tags    runs
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   playbook query string false "Only runs of this playbook"
// @Param   host query string false "Only runs that mention this host"
// @Param   from query string false "Inclusive RFC3339 start time"
// @Param   to query string false "Exclusive RFC3339 end time"
// @Param   limit query int false "Maximum number of runs" default(50)
// @Param   offset query int false "Number of matching runs to skip" default(0)
// @Success 200 {array} store.RunRecord "Runs"
// @Header  200 {integer} X-Total-Count "Total matching runs before pagination"
// @Failure 400 {object} apiErrorDoc "invalid filter"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/runs [get]
func (s *Server) docRuns() {}

// docCollectionRuns godoc
//
// @ID      get_orgs_org_collections_collection_runs
// @Summary List runs of this collection
// @Tags    runs
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   playbook query string false "Only runs of this playbook"
// @Param   host query string false "Only runs that mention this host"
// @Param   from query string false "Inclusive RFC3339 start time"
// @Param   to query string false "Exclusive RFC3339 end time"
// @Param   limit query int false "Maximum number of runs" default(50)
// @Param   offset query int false "Number of matching runs to skip" default(0)
// @Success 200 {array} store.RunRecord "Runs"
// @Header  200 {integer} X-Total-Count "Total matching runs before pagination"
// @Failure 400 {object} apiErrorDoc "invalid filter"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/runs [get]
func (s *Server) docCollectionRuns() {}

// docRunDetail godoc
//
// @ID      get_runs_id
// @Summary Run details including captured output
// @Tags    runs
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   id path int true "Numeric run identifier"
// @Success 200 {object} store.RunRecord "Run"
// @Failure 400 {object} apiErrorDoc "invalid run id"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 404 {object} apiErrorDoc "run not found"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/runs/{id} [get]
func (s *Server) docRunDetail() {}

// docCollectionRunDetail godoc
//
// @ID      get_orgs_org_collections_collection_runs_id
// @Summary Run details for this collection
// @Tags    runs
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   id path int true "Numeric run identifier"
// @Success 200 {object} store.RunRecord "Run"
// @Failure 400 {object} apiErrorDoc "invalid run id"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "run not found"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/runs/{id} [get]
func (s *Server) docCollectionRunDetail() {}

// docRunTasks godoc
//
// @ID      get_orgs_org_collections_collection_runs_id_tasks
// @Summary Per-task history of a run
// @Tags    runs
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   id path int true "Numeric run identifier"
// @Success 200 {array} store.TaskRunRecord "Task runs"
// @Failure 400 {object} apiErrorDoc "invalid run id"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "run not found"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/runs/{id}/tasks [get]
func (s *Server) docRunTasks() {}

// docHosts godoc
//
// @ID      get_hosts
// @Summary List known hosts
// @Tags    hosts
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Success 200 {array} store.HostRecord "Hosts"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/hosts [get]
func (s *Server) docHosts() {}

// docCollectionHosts godoc
//
// @ID      get_orgs_org_collections_collection_hosts
// @Summary Hosts of this collection
// @Tags    hosts
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Success 200 {array} store.HostRecord "Hosts"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hosts [get]
func (s *Server) docCollectionHosts() {}

// docHostRuns godoc
//
// @ID      get_hosts_host_runs
// @Summary Run history of one host
// @Tags    hosts
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   host path string true "Inventory host name"
// @Param   limit query int false "Maximum number of entries" default(10)
// @Success 200 {array} store.HostRunRecord "Host runs"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/hosts/{host}/runs [get]
func (s *Server) docHostRuns() {}

// docCollectionHostRuns godoc
//
// @ID      get_orgs_org_collections_collection_hosts_host_runs
// @Summary Run history of one host in this collection
// @Tags    hosts
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   host path string true "Inventory host name"
// @Param   limit query int false "Maximum number of entries" default(10)
// @Success 200 {array} store.HostRunRecord "Host runs"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hosts/{host}/runs [get]
func (s *Server) docCollectionHostRuns() {}

// docHostChanges godoc
//
// @ID      get_hosts_host_changes
// @Summary Latest changes of one host
// @Tags    hosts
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   host path string true "Inventory host name"
// @Param   limit query int false "Maximum number of entries" default(200)
// @Success 200 {array} store.TaskStateRecord "Task changes"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/hosts/{host}/changes [get]
func (s *Server) docHostChanges() {}

// docCollectionHostChanges godoc
//
// @ID      get_orgs_org_collections_collection_hosts_host_changes
// @Summary Latest changes of one host in this collection
// @Tags    hosts
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   host path string true "Inventory host name"
// @Param   limit query int false "Maximum number of entries" default(200)
// @Success 200 {array} store.TaskStateRecord "Task changes"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hosts/{host}/changes [get]
func (s *Server) docCollectionHostChanges() {}

// docPlaybooks godoc
//
// @ID      get_playbooks
// @Summary Configured playbooks grouped by collection
// @Tags    playbooks
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Success 200 {object} map[string][]playbookInfo "Playbooks"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/playbooks [get]
func (s *Server) docPlaybooks() {}

// docCollectionPlaybooks godoc
//
// @ID      get_orgs_org_collections_collection_playbooks
// @Summary Playbooks of this collection
// @Tags    playbooks
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Success 200 {object} map[string][]playbookInfo "Playbooks"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/playbooks [get]
func (s *Server) docCollectionPlaybooks() {}

// docWorkers godoc
//
// @ID      get_workers
// @Summary Worker pool status
// @Tags    workers
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Success 200 {array} worker.WorkerStatus "Workers"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Router  /api/v1/workers [get]
func (s *Server) docWorkers() {}

// docRun godoc
//
// @ID      post_run
// @Summary Queue a playbook run (legacy alias for collection runs)
// @Tags    runs
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   request body runRequestDoc true "run request"
// @Success 202 {object} runAcceptedResponseDoc "Queued"
// @Failure 400 {object} apiErrorDoc "invalid request"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown playbook"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/run [post]
func (s *Server) docRun() {}

// docCollectionRun godoc
//
// @ID      post_orgs_org_collections_collection_run
// @Summary Queue a playbook run in this collection
// @Tags    runs
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   request body runRequestDoc true "run request"
// @Success 202 {object} runAcceptedResponseDoc "Queued"
// @Failure 400 {object} apiErrorDoc "invalid request"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown playbook"
// @Failure 500 {object} apiErrorDoc "store error"
// @Router  /api/v1/orgs/{org}/collections/{collection}/run [post]
func (s *Server) docCollectionRun() {}

// docCancelRun godoc
//
// @ID      post_orgs_org_collections_collection_runs_id_cancel
// @Summary Cancel a queued or running job
// @Tags    runs
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   id path int true "Numeric run identifier"
// @Success 200 {object} cancelRunResponseDoc "Cancellation requested"
// @Failure 400 {object} apiErrorDoc "invalid run id"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "run not found"
// @Failure 409 {object} apiErrorDoc "run is not active"
// @Failure 500 {object} apiErrorDoc "store error"
// @Failure 501 {object} apiErrorDoc "cancellation unavailable"
// @Router  /api/v1/orgs/{org}/collections/{collection}/runs/{id}/cancel [post]
func (s *Server) docCancelRun() {}

// docSync godoc
//
// @ID      post_sync
// @Summary Synchronize a git-backed collection
// @Tags    collections
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Security syncToken
// @Param   request body syncRequestDoc true "sync request"
// @Success 200 {object} syncResponseDoc "Synchronized"
// @Failure 400 {object} apiErrorDoc "invalid request or not a git collection"
// @Failure 401 {object} apiErrorDoc "missing or invalid credentials"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown target"
// @Failure 502 {object} apiErrorDoc "git failure"
// @Router  /api/v1/sync [post]
func (s *Server) docSync() {}

// docCollectionSync godoc
//
// @ID      post_orgs_org_collections_collection_sync
// @Summary Synchronize this collection
// @Tags    collections
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Security syncToken
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   request body syncRequestDoc true "sync request"
// @Success 200 {object} syncResponseDoc "Synchronized"
// @Failure 400 {object} apiErrorDoc "invalid request or not a git collection"
// @Failure 401 {object} apiErrorDoc "missing or invalid credentials"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown target"
// @Failure 502 {object} apiErrorDoc "git failure"
// @Router  /api/v1/orgs/{org}/collections/{collection}/sync [post]
func (s *Server) docCollectionSync() {}

// docOrgs godoc
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
func (s *Server) docOrgs() {}

// docOrg godoc
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
func (s *Server) docOrg() {}

// docCollections godoc
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
func (s *Server) docCollections() {}

// docCollection godoc
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
func (s *Server) docCollection() {}

// docHostKeys godoc
//
// @ID      get_orgs_org_collections_collection_hostkeys
// @Summary Pending SSH host keys of this collection
// @Tags    ssh
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Success 200 {array} sshpool.HostKey "Host keys"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hostkeys [get]
func (s *Server) docHostKeys() {}

// docSSHStats godoc
//
// @ID      get_orgs_org_collections_collection_ssh_stats
// @Summary SSH connection pool counters
// @Tags    ssh
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Success 200 {object} map[string]sshpool.Stats "Pool stats"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Router  /api/v1/orgs/{org}/collections/{collection}/ssh/stats [get]
func (s *Server) docSSHStats() {}

// docAcceptHostKey godoc
//
// @ID      post_orgs_org_collections_collection_hostkeys_accept
// @Summary Approve a pending SSH host key
// @Tags    ssh
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   request body hostKeyDecisionRequestDoc true "host key decision"
// @Success 200 {object} hostKeyDecisionResponseDoc "Approved"
// @Failure 400 {object} apiErrorDoc "invalid decision"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hostkeys/accept [post]
func (s *Server) docAcceptHostKey() {}

// docRejectHostKey godoc
//
// @ID      post_orgs_org_collections_collection_hostkeys_reject
// @Summary Reject a pending SSH host key
// @Tags    ssh
// @Produce json
// @Security sessionCookie
// @Security userBearer
// @Param   org path string true "Organization name"
// @Param   collection path string true "Collection (target) name"
// @Param   request body hostKeyDecisionRequestDoc true "host key decision"
// @Success 200 {object} hostKeyDecisionResponseDoc "Rejected"
// @Failure 400 {object} apiErrorDoc "invalid decision"
// @Failure 401 {object} apiErrorDoc "not authenticated"
// @Failure 403 {object} apiErrorDoc "access denied"
// @Failure 404 {object} apiErrorDoc "unknown collection"
// @Router  /api/v1/orgs/{org}/collections/{collection}/hostkeys/reject [post]
func (s *Server) docRejectHostKey() {}

// docOpenAPISpec godoc
//
// @ID      get_openapi_json
// @Summary OpenAPI 3 description of the versioned API
// @Tags    system
// @Produce json
// @Success 200 {object} map[string]any "OpenAPI document"
// @Router  /api/v1/openapi.json [get]
func (s *Server) docOpenAPISpec() {}

// docSwaggerUI godoc
//
// @ID      get_api_docs
// @Summary Swagger UI for the versioned API
// @Tags    system
// @Produce html
// @Success 200 {string} string "Swagger UI"
// @Router  /api/docs [get]
func (s *Server) docSwaggerUI() {}
