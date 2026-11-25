package server

import (
	"net/http"

	"github.com/kakwa/strangle/internal/server/handlers"
)

// apiVersion is the current version of the HTTP API. Every API route lives
// under /api/v1; flat /api/* aliases have been removed.
const apiVersion = "v1"

// apiPrefix is the versioned prefix of every API route.
const apiPrefix = "/api/" + apiVersion

// routeRegistrar is the registration surface shared by the Echo adapter and
// tests. Patterns are "METHOD /path".
type routeRegistrar interface {
	HandleFunc(pattern string, handler http.HandlerFunc)
}

// api builds the handler dependencies shared by the route table and the
// collection-access middleware.
func (s *Server) api() *handlers.API {
	return &handlers.API{
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
// implementations (and their swaggo annotations) live in the handlers package;
// this file is the single source of truth for the URL layout. Collection
// access is enforced by middleware, not by per-route wrappers.
func (s *Server) registerAPIRoutes(routes routeRegistrar, api *handlers.API) {
	// --- system / authentication -------------------------------------------
	routes.HandleFunc("GET "+apiPrefix+"/health", api.Health)
	routes.HandleFunc("POST "+apiPrefix+"/login", api.Login)
	routes.HandleFunc("POST "+apiPrefix+"/logout", api.Logout)
	routes.HandleFunc("GET "+apiPrefix+"/me", api.Me)
	// GET /api/v1/openapi.json is registered with the docs routes in server.go.

	// --- organizations and collections -------------------------------------
	routes.HandleFunc("GET "+apiPrefix+"/orgs", api.ListOrgs)
	routes.HandleFunc("POST "+apiPrefix+"/orgs", api.CreateOrg)
	routes.HandleFunc("GET "+apiPrefix+"/orgs/{org}", api.GetOrg)
	routes.HandleFunc("GET "+apiPrefix+"/orgs/{org}/collections", api.ListCollections)
	routes.HandleFunc("GET "+apiPrefix+"/orgs/{org}/collections/{collection}", api.GetCollection)

	// --- collection resources ----------------------------------------------
	routes.HandleFunc("GET "+apiPrefix+"/orgs/{org}/collections/{collection}/hosts", api.ListHosts)
	routes.HandleFunc("GET "+apiPrefix+"/orgs/{org}/collections/{collection}/playbooks", api.CollectionPlaybooks)
	routes.HandleFunc("POST "+apiPrefix+"/orgs/{org}/collections/{collection}/hook", api.Hook)
	routes.HandleFunc("GET "+apiPrefix+"/orgs/{org}/collections/{collection}/tokens", api.ListTokens)
	routes.HandleFunc("POST "+apiPrefix+"/orgs/{org}/collections/{collection}/tokens", api.CreateToken)
	routes.HandleFunc("DELETE "+apiPrefix+"/orgs/{org}/collections/{collection}/tokens/{name}", api.DeleteToken)

	// --- playbook-scoped host resources ------------------------------------
	base := apiPrefix + "/orgs/{org}/collections/{collection}/playbooks/{playbook}/hosts/{host}"
	routes.HandleFunc("GET "+base+"/hostkeys", api.HostKeys)
	routes.HandleFunc("PATCH "+base, api.PatchHost)
	routes.HandleFunc("GET "+base+"/runs", api.HostRuns)
}
