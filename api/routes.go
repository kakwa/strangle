package api

import (
	"github.com/labstack/echo/v5"
)

// apiVersion is the current version of the HTTP API. Every API route lives
// under /api/v1; flat /api/* aliases have been removed.
const apiVersion = "v1"

// apiPrefix is the versioned prefix of every API route.
const apiPrefix = "/api/" + apiVersion

// api builds the handler dependencies shared by the route table and the
// collection-access middleware.
func (s *Server) api() *API {
	return &API{
		Applier:   s.applier,
		Store:     s.store,
		SSH:       s.ssh,
		Syncer:    s.syncer,
		Users:     s.users,
		Tokens:    s.tokens,
		Sessions:  s.sessions,
		Access:    s.access,
		AuthOn:    s.authOn,
		AuthToken: s.authToken,
		Logger:    s.logger,
	}
}

// registerAPIRoutes wires the complete versioned route table. Handler
// implementations (and their swaggo annotations) live in this package; this
// file is the single source of truth for the URL layout. Collection access
// is enforced by middleware, not by per-route wrappers.
func (s *Server) registerAPIRoutes(e *echo.Echo, api *API) {
	// --- system / authentication -------------------------------------------
	e.GET(apiPrefix+"/health", api.Health)
	e.POST(apiPrefix+"/login", api.Login)
	e.POST(apiPrefix+"/logout", api.Logout)
	e.GET(apiPrefix+"/me", api.Me)
	// GET /api/v1/openapi.json is registered with the docs routes in server.go.

	// --- organizations and collections -------------------------------------
	e.GET(apiPrefix+"/orgs", api.ListOrgs)
	e.POST(apiPrefix+"/orgs", api.CreateOrg)
	e.GET(apiPrefix+"/orgs/:org", api.GetOrg)
	e.GET(apiPrefix+"/orgs/:org/collections", api.ListCollections)
	e.GET(apiPrefix+"/orgs/:org/collections/:collection", api.GetCollection)

	// --- collection resources ----------------------------------------------
	coll := e.Group(apiPrefix + "/orgs/:org/collections/:collection")
	coll.GET("/hosts", api.ListHosts)
	coll.GET("/playbooks", api.CollectionPlaybooks)
	coll.POST("/hook", api.Hook)
	coll.GET("/tokens", api.ListTokens)
	coll.POST("/tokens", api.CreateToken)
	coll.DELETE("/tokens/:name", api.DeleteToken)

	// --- playbook-scoped host resources ------------------------------------
	host := e.Group(apiPrefix + "/orgs/:org/collections/:collection/playbooks/:playbook/hosts/:host")
	host.GET("/hostkeys", api.HostKeys)
	host.GET("/runs", api.HostRuns)
	host.PATCH("", api.PatchHost)
}
