// Package api implements the versioned Strangle HTTP API: the handlers and
// their swaggo annotations, the route table, and the transport wiring
// (authentication middleware, metrics, docs, static UI) around them.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/security/auth"
	"github.com/kakwa/strangle/utils/cfg"
	"github.com/kakwa/strangle/utils/sshpool"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap"
)

// API holds the dependencies shared by all handlers. It is constructed by the
// server and used by routes.go to register the route table.
type API struct {
	Applier   *applier.Ansible
	Store     model.Store
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

// apiError is the uniform error body of the versioned API.
type apiError struct {
	Error string `json:"error"`
}

// statusError pairs an HTTP status with the error to report. Handlers return
// it; the server's error handler writes the uniform error body.
type statusError struct {
	code int
	err  error
}

func (e *statusError) Error() string { return e.err.Error() }

func (e *statusError) Unwrap() error { return e.err }

func apiStatus(code int, err error) *statusError {
	return &statusError{code: code, err: err}
}

// apiErrorHandler writes the uniform error body for a statusError (or an
// Echo HTTPError), with a 500 fallback for anything else.
func apiErrorHandler(c *echo.Context, err error) {
	code := http.StatusInternalServerError
	var se *statusError
	if errors.As(err, &se) {
		code = se.code
	} else if he, ok := err.(*echo.HTTPError); ok {
		code = he.Code
	}
	_ = c.JSON(code, apiError{Error: err.Error()})
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

// requireCollection resolves the org/collection of a route, answering a 404
// status error for an unknown pair.
func (a *API) requireCollection(c *echo.Context) (cfg.TargetConfig, error) {
	org, coll := c.Param("org"), c.Param("collection")
	t, ok := a.collectionIn(org, coll)
	if !ok {
		return cfg.TargetConfig{}, apiStatus(http.StatusNotFound, unknownCollection(org, coll))
	}
	return t, nil
}

// collectionScope returns the collection of a scoped route, or a 404 status
// error for an unknown pair. Every registered collection route has both
// parameters.
func (a *API) collectionScope(c *echo.Context) (string, error) {
	org, coll := c.Param("org"), c.Param("collection")
	if coll == "" {
		return "", nil
	}
	if _, ok := a.collectionIn(org, coll); !ok {
		return "", apiStatus(http.StatusNotFound, unknownCollection(org, coll))
	}
	return coll, nil
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
	// scoped limits a bearer token to the collections it was created for;
	// nil means the caller is not scope-limited.
	var scoped map[string]bool
	if len(identity.TokenScopes) > 0 {
		scoped = make(map[string]bool, len(identity.TokenScopes))
		for _, s := range identity.TokenScopes {
			scoped[s.Collection] = true
		}
	}
	if !a.restricted(ctx) {
		return scoped
	}
	user, ok := UserFrom(r)
	if !ok {
		// No resolvable user: a scoped token keeps its own scope, anything
		// else sees nothing.
		if scoped == nil {
			return map[string]bool{}
		}
		return scoped
	}
	memberships, err := a.Access.List(ctx, user)
	if err != nil {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(memberships))
	for _, m := range memberships {
		if scoped == nil || scoped[m.Collection] {
			out[m.Collection] = true
		}
	}
	return out
}

// targetVisible reports whether a record of collection target is visible.
func targetVisible(allowed map[string]bool, target string) bool {
	return allowed == nil || allowed[target]
}

// MayUse reports whether the caller may access one org/collection pair.
// A scoped bearer token only works for its own pair; the static token is a
// machine identity allowed on the hook route only. It is used by the
// server's collection-access middleware.
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
func (a *API) playbookScope(c *echo.Context) (string, error) {
	if _, err := a.requireCollection(c); err != nil {
		return "", err
	}
	collection, playbook := c.Param("collection"), c.Param("playbook")
	if a.Applier != nil && !a.Applier.KnownPlaybook(collection, playbook) {
		return "", apiStatus(http.StatusNotFound, fmt.Errorf("unknown playbook %q for collection %q", playbook, collection))
	}
	return collection + "/" + playbook, nil
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
