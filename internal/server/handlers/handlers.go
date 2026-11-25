// Package handlers implements the versioned Strangle HTTP API.
//
// The server package owns transport wiring (authentication middleware,
// metrics, docs, static UI) and the route table; this package owns the
// handlers themselves and the swaggo annotations that document them.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/cfg"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/sshpool"
	"go.uber.org/zap"
)

// Syncer syncs a git-backed collection target and reports the resolved
// configs so the applier can refresh its view after a hook.
type Syncer interface {
	Sync(ctx context.Context, target, branch string) (string, bool, error)
	Targets() []cfg.TargetConfig
}

// API holds the dependencies shared by all handlers. It is constructed by the
// server package and used by routes.go to register the route table.
type API struct {
	Applier   *applier.Ansible
	Store     db.Store
	SSH       *sshpool.Multi
	Syncer    Syncer
	Users     auth.UserStore
	Tokens    auth.TokenStore
	Sessions  *auth.Sessions
	Access    auth.CollectionAccess
	AuthOn    bool
	AuthToken string
	Logger    *zap.Logger
}

// Auth is the identity resolved for one request by the server's auth
// middleware. TokenScopes narrows a bearer token's access to the collections
// it was created for; an empty slice means an unscoped (CLI) token.
type Auth struct {
	User        string
	SessionUser string
	StaticToken bool
	TokenScopes []TokenScope
}

// TokenScope is one org/collection pair a bearer token may access.
type TokenScope struct {
	Org        string
	Collection string
}

type authKey struct{}

// WithAuth attaches the resolved identity to a request context.
func WithAuth(ctx context.Context, a Auth) context.Context {
	return context.WithValue(ctx, authKey{}, a)
}

// FromRequest returns the identity attached by the auth middleware. The zero
// value is returned when the middleware did not run (unit tests calling a
// handler directly).
func FromRequest(r *http.Request) Auth {
	if a, ok := r.Context().Value(authKey{}).(Auth); ok {
		return a
	}
	return Auth{}
}

// UserFrom returns the authenticated user (session or bearer token owner).
func UserFrom(r *http.Request) (string, bool) {
	a := FromRequest(r)
	return a.User, a.User != ""
}

// SessionUserFrom returns the user authenticated by a UI session, if any.
func SessionUserFrom(r *http.Request) (string, bool) {
	a := FromRequest(r)
	return a.SessionUser, a.SessionUser != ""
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the API's uniform error body.
func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, apiErrorDoc{Error: err.Error()})
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func intQuery(r *http.Request, name string, def int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// orgOf returns the organization a configured collection belongs to.
func orgOf(t cfg.TargetConfig) string {
	if t.Org == "" {
		return cfg.DefaultOrg
	}
	return t.Org
}

// collectionIn resolves a collection by org and name.
func (a *API) collectionIn(org, name string) (cfg.TargetConfig, bool) {
	if a.Applier == nil {
		return cfg.TargetConfig{}, false
	}
	t, ok := a.Applier.Target(name)
	if !ok || orgOf(t) != org {
		return cfg.TargetConfig{}, false
	}
	return t, true
}

// requireCollection answers 404 for an unknown org/collection pair.
func (a *API) requireCollection(w http.ResponseWriter, r *http.Request) (cfg.TargetConfig, bool) {
	t, ok := a.collectionIn(r.PathValue("org"), r.PathValue("collection"))
	if !ok {
		writeError(w, http.StatusNotFound, unknownCollection(r.PathValue("org"), r.PathValue("collection")))
	}
	return t, ok
}

// collectionScope returns the collection of a scoped route, or ok=false for
// an unknown pair. Every registered collection route has both parameters.
func (a *API) collectionScope(r *http.Request) (string, bool) {
	name := r.PathValue("collection")
	if name == "" {
		return "", true
	}
	if _, ok := a.collectionIn(r.PathValue("org"), name); !ok {
		return "", false
	}
	return name, true
}

func unknownCollection(org, collection string) error {
	return fmt.Errorf("unknown collection %q in org %q", collection, org)
}

// restricted reports whether collection memberships are in effect.
func (a *API) restricted(ctx context.Context) bool {
	if !a.AuthOn || a.Access == nil {
		return false
	}
	any, err := a.Access.HasAny(ctx)
	if err != nil {
		return true
	}
	return any
}

// accessibleTargets returns the collections the caller may see. A nil set
// means unrestricted; an empty set means nothing is visible. Bearer tokens
// are additionally limited to the collections they were created for.
func (a *API) accessibleTargets(ctx context.Context, r *http.Request) map[string]bool {
	identity := FromRequest(r)
	if len(identity.TokenScopes) > 0 {
		scoped := make(map[string]bool, len(identity.TokenScopes))
		for _, s := range identity.TokenScopes {
			scoped[s.Collection] = true
		}
		user, ok := UserFrom(r)
		if !ok || !a.restricted(ctx) {
			return scoped
		}
		memberships, err := a.Access.List(ctx, user)
		if err != nil {
			return map[string]bool{}
		}
		out := make(map[string]bool, len(scoped))
		for _, m := range memberships {
			if scoped[m.Collection] {
				out[m.Collection] = true
			}
		}
		return out
	}
	if !a.restricted(ctx) {
		return nil
	}
	user, ok := UserFrom(r)
	if !ok {
		return map[string]bool{}
	}
	memberships, err := a.Access.List(ctx, user)
	if err != nil {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(memberships))
	for _, m := range memberships {
		out[m.Collection] = true
	}
	return out
}

// targetVisible reports whether a record of collection target is visible.
func targetVisible(allowed map[string]bool, target string) bool {
	return allowed == nil || allowed[target]
}

// MayUse reports whether the caller may access one org/collection pair.
// A scoped bearer token only works for its own pair; the static token is a
// machine identity allowed on the hook route only. It is exported so the
// server's collection-access middleware can apply the same check.
func (a *API) MayUse(ctx context.Context, r *http.Request, org, collection string) bool {
	identity := FromRequest(r)
	if len(identity.TokenScopes) > 0 {
		found := false
		for _, s := range identity.TokenScopes {
			if s.Org == org && s.Collection == collection {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if !a.restricted(ctx) {
		return true
	}
	if user, ok := UserFrom(r); ok {
		allowed, err := a.Access.Allows(ctx, user, org, collection)
		return err == nil && allowed
	}
	return identity.StaticToken && hookPath(r)
}

// hookPath reports whether r is the collection hook route. The static
// machine token is only accepted there, never on a regular API route.
func hookPath(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/hook")
}

// playbookScope returns the SSH scope key of the playbook named in the URL
// ("<collection>/<playbook>"), after validating both path components.
func (a *API) playbookScope(w http.ResponseWriter, r *http.Request) (string, bool) {
	if _, ok := a.requireCollection(w, r); !ok {
		return "", false
	}
	collection, playbook := r.PathValue("collection"), r.PathValue("playbook")
	if a.Applier != nil && !a.Applier.KnownPlaybook(collection, playbook) {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown playbook %q for collection %q", playbook, collection))
		return "", false
	}
	return collection + "/" + playbook, true
}

// organizations returns every organization: stored ones (created through the
// API) and ones derived from the configuration, merged by name.
func (a *API) organizations(ctx context.Context) []orgInfo {
	byOrg := map[string][]collectionInfo{}
	if a.Applier != nil {
		for _, name := range a.Applier.TargetNames() {
			t, ok := a.Applier.Target(name)
			if !ok {
				continue
			}
			org := orgOf(t)
			byOrg[org] = append(byOrg[org], collectionInfo{
				Name:      name,
				Org:       org,
				Playbooks: playbookInfos(t.Playbooks),
			})
		}
	}
	if a.Store != nil {
		stored, err := a.Store.ListOrgs(ctx)
		if err == nil {
			for _, o := range stored {
				if _, ok := byOrg[o.Name]; !ok {
					byOrg[o.Name] = []collectionInfo{}
				}
			}
		}
	}
	names := make([]string, 0, len(byOrg))
	for name := range byOrg {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]orgInfo, 0, len(names))
	for _, name := range names {
		colls := byOrg[name]
		sort.Slice(colls, func(i, j int) bool { return colls[i].Name < colls[j].Name })
		out = append(out, orgInfo{Name: name, Collections: colls})
	}
	return out
}

func playbookInfos(configs []cfg.PlaybookConfig) []playbookInfo {
	pbs := make([]playbookInfo, 0, len(configs))
	for _, pb := range configs {
		pbs = append(pbs, playbookInfo{
			Name:      pb.Name,
			Playbook:  strings.TrimPrefix(pb.Playbook, "./"),
			Inventory: strings.TrimPrefix(pb.Inventory, "./"),
		})
	}
	return pbs
}

// visibleOrganizations filters organizations to the collections the caller
// may see. Empty organizations (created via the API) stay visible.
func (a *API) visibleOrganizations(r *http.Request) []orgInfo {
	all := a.organizations(r.Context())
	allowed := a.accessibleTargets(r.Context(), r)
	if allowed == nil {
		return all
	}
	// A scoped bearer token never learns about organizations outside its own
	// scope, even when those organizations have no configured collections.
	identity := FromRequest(r)
	scopeOrgs := map[string]bool{}
	for _, s := range identity.TokenScopes {
		scopeOrgs[s.Org] = true
	}
	out := make([]orgInfo, 0, len(all))
	for _, o := range all {
		if len(identity.TokenScopes) > 0 && !scopeOrgs[o.Name] {
			continue
		}
		colls := make([]collectionInfo, 0, len(o.Collections))
		for _, c := range o.Collections {
			if allowed[c.Name] {
				colls = append(colls, c)
			}
		}
		if len(o.Collections) == 0 || len(colls) > 0 {
			o.Collections = colls
			out = append(out, o)
		}
	}
	return out
}

// collectionLinks builds the sub-resource URLs advertised by the collection
// detail endpoint. Removed endpoints are no longer advertised.
func collectionLinks(org, collection string) map[string]string {
	base := "/api/v1/orgs/" + org + "/collections/" + collection
	return map[string]string{
		"hosts":     base + "/hosts",
		"playbooks": base + "/playbooks",
		"tokens":    base + "/tokens",
		"hook":      base + "/hook",
	}
}
