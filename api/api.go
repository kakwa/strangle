// Package api implements the versioned Strangle HTTP API: the handlers and
// their swaggo annotations, the route table, and the transport wiring
// (authentication middleware, docs, static UI) around them.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/security/auth"
	"github.com/kakwa/strangle/ui"
	"github.com/kakwa/strangle/utils/sshpool"
	"github.com/labstack/echo/v5"
	echoSwagger "github.com/swaggo/echo-swagger/v2"
	swag "github.com/swaggo/swag/v2"
	"go.uber.org/zap"

	// Register the swag-generated OpenAPI document via its init().
	_ "github.com/kakwa/strangle/api/docs"
)

const (
	jsonMedia = "application/json"
	htmlMedia = "text/html"
)

// RunEnqueuer accepts a request to run one playbook now by enqueueing a River
// job. The HTTP server no longer exposes manual run endpoints, but the queue
// is still used by the scheduler in cmd/strangle-server.
type RunEnqueuer interface {
	EnqueueRun(ctx context.Context, target, playbook string, runID uint64, check bool) (string, error)
}

// Syncer syncs a git-backed collection target. It is nil when no target has a
// git upstream.
type Syncer interface {
	Sync(ctx context.Context, target, branch string) (string, bool, error)
	// Targets reports the resolved target configs, so the applier's view
	// can be refreshed after a hook.
	Targets() []cfg.TargetConfig
}

// Server is the strangle HTTP server.
type Option func(*Server)

// WithCollectionAccess enables per-collection authorization. Without it the
// API does not restrict collections by user.
func WithCollectionAccess(a auth.CollectionAccess) Option {
	return func(s *Server) { s.access = a }
}

// WithTLS enables HTTPS on ListenAndServe. Both files must be set; a single
// file is a configuration error reported when the server starts.
func WithTLS(certFile, keyFile string) Option {
	return func(s *Server) {
		s.tlsCertFile = certFile
		s.tlsKeyFile = keyFile
	}
}

type Server struct {
	addr        string
	applier     *applier.Ansible
	store       model.Store
	ssh         *sshpool.Multi
	syncer      Syncer
	users       auth.UserStore
	tokens      auth.TokenStore
	sessions    *auth.Sessions
	authOn      bool
	authToken   string
	access      auth.CollectionAccess
	tlsCertFile string
	tlsKeyFile  string
	logger      *zap.Logger
}

// uiFS is the built web UI, embedded in the binary at compile time
// (see package ui), rooted at ui/dist.
var uiFS = func() fs.FS {
	ui, err := fs.Sub(ui.FS, "dist")
	if err != nil {
		// dist is embedded at compile time and always present.
		panic(err)
	}
	return ui
}()

// New returns a Server. The web UI is embedded in the binary; build it with
// `make ui` (or `make build`) before compiling. ssh may be nil when no
// target uses the Go-managed SSH transport. syncer may be nil when no target
// has a git upstream. users, when non-nil, is the store of UI accounts; UI
// authentication is enforced as soon as it holds at least one user (see
// Routes). tokens, when non-nil, is the store of collection-scoped user API
// tokens. authToken, when non-empty, is a static bearer token accepted on the
// collection hook only. The run argument is kept for source compatibility
// and ignored.
func New(addr string, apr *applier.Ansible, st model.Store, ssh *sshpool.Multi, _ RunEnqueuer, syncer Syncer, users auth.UserStore, tokens auth.TokenStore, authToken string, logger *zap.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Server{
		addr:      addr,
		applier:   apr,
		store:     st,
		ssh:       ssh,
		syncer:    syncer,
		users:     users,
		tokens:    tokens,
		authToken: authToken,
		logger:    logger,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Routes builds the HTTP handler: the /api/v1 endpoints, the API docs and
// the embedded web UI.
//
// UI authentication is enforced when a user store is configured and holds at
// least one user: every /api/* request then needs a valid session or user
// token, except the login endpoints, GET /api/v1/health (liveness), the docs
// and GET /api/v1/openapi.json. Without users the server runs open and logs a
// warning pointing at `strangle-cli user add`.
func (s *Server) Routes() http.Handler {
	return s.buildRoutes()
}

// buildRoutes registers every route on an Echo instance and returns it. Echo
// implements http.Handler, so tests and the net/http server can use it
// directly.
func (s *Server) buildRoutes() *echo.Echo {
	s.initAuth()

	e := echo.New()
	// Clients (git hooks, the CLI, tests) post raw JSON bodies without a
	// Content-Type header, so decode as JSON whenever the header is absent or
	// a JSON media type; anything else falls back to Echo's default binder.
	e.Binder = lenientJSONBinder{}
	e.HTTPErrorHandler = apiErrorHandler
	api := s.api()
	e.Use(s.authMiddleware, collectionAccessMiddleware(api))

	s.registerAPIRoutes(e, api)

	// Machine-readable API description and Swagger UI. Both are public: the
	// spec describes the routes but exposes no collection data itself.
	s.registerDocsRoutes(e)

	// The UI owns every path not claimed by an API route.
	e.Any("/*", echo.WrapHandler(spaHandler(uiFS)))
	return e
}

// lenientJSONBinder implements echo.Binder: JSON whenever the Content-Type is
// missing or a JSON media type, otherwise the default binder.
type lenientJSONBinder struct{}

func (lenientJSONBinder) Bind(c *echo.Context, target any) error {
	ctype := c.Request().Header.Get("Content-Type")
	if ctype == "" || strings.HasPrefix(ctype, "application/json") || strings.HasSuffix(ctype, "+json") {
		return json.NewDecoder(c.Request().Body).Decode(target)
	}
	return (&echo.DefaultBinder{}).Bind(c, target)
}

// registerDocsRoutes serves the swag-generated OpenAPI 3.1 document and the
// embedded Swagger UI. Both are public: the spec describes the routes but
// exposes no collection data itself.
func (s *Server) registerDocsRoutes(e *echo.Echo) {
	e.GET(apiPrefix+"/openapi.json", func(c *echo.Context) error {
		doc, err := swag.ReadDoc()
		if err != nil {
			return c.JSON(http.StatusInternalServerError, apiError{Error: err.Error()})
		}
		return c.Blob(http.StatusOK, jsonMedia, []byte(doc))
	})

	ui := echoSwagger.EchoWrapHandlerV3(echoSwagger.URL(apiPrefix + "/openapi.json"))
	// echo-swagger expects /api/docs/index.html; /api/docs is the documented
	// short form, so serve the same handler with a rewritten RequestURI.
	e.GET("/api/docs", func(c *echo.Context) error {
		r := c.Request()
		saved := r.RequestURI
		r.RequestURI = "/api/docs/index.html"
		defer func() { r.RequestURI = saved }()
		return ui(c)
	})
	e.GET("/api/docs/*", ui)
}

// initAuth sets up the session table and decides, once, whether UI
// authentication is enforced: a user store must be configured and hold at
// least one user. Without that the API runs open (dev mode, fresh install).
// It is idempotent, so Routes() can be called repeatedly.
func (s *Server) initAuth() {
	if s.sessions != nil || s.users == nil {
		return
	}
	s.sessions = auth.NewSessions()
	n, err := s.users.Count(context.Background())
	switch {
	case err != nil:
		s.logger.Warn("cannot count users; running open", zap.Error(err))
	case n == 0:
		s.logger.Warn("no users configured; the API is open. Create the first account with: strangle-cli user add <name> --password <pw>")
	default:
		s.authOn = true
		s.logger.Info("UI authentication enabled", zap.Int("users", n))
	}
}

// resolveAuth checks the credentials carried by r. A session wins over a
// bearer token; both are checked before the static token so a token endpoint
// never mistakes the machine credential for a user. Collection-scoped user
// tokens add a scope restriction to the resolved identity.
func (s *Server) resolveAuth(r *http.Request) Auth {
	var a Auth
	if sess, ok := s.sessionFrom(r); ok {
		a.User = sess.Username
		a.SessionUser = sess.Username
	} else if raw, ok := bearerTokenValue(r); ok && s.tokens != nil {
		if tok, err := s.tokens.Lookup(r.Context(), raw); err == nil {
			a.User = tok.Owner
			if tok.Org != "" || tok.Collection != "" {
				a.TokenScopes = []TokenScope{{Org: tok.Org, Collection: tok.Collection}}
			}
		}
	}
	a.StaticToken = s.bearerTokenOK(r)
	return a
}

// hookOK reports whether r may bypass the ordinary user requirement. The
// static sync token is only accepted on the collection hook route.
func (s *Server) hookOK(r *http.Request) bool {
	if !hookPath(r) {
		return false
	}
	a := FromRequest(r)
	return a.User != "" || a.StaticToken || (!s.authOn && s.authToken == "")
}

// authOpen lists the API endpoints reachable without a session.
func (s *Server) authOpen(r *http.Request) bool {
	// API docs are public but have no versioned alias beyond the spec itself.
	// Swagger UI assets live under /api/docs/ and must be public too.
	if r.Method == http.MethodGet && (r.URL.Path == "/api/docs" ||
		strings.HasPrefix(r.URL.Path, "/api/docs/") ||
		r.URL.Path == apiPrefix+"/openapi.json") {
		return true
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == apiPrefix+"/health":
		return true
	case r.Method == http.MethodPost && r.URL.Path == apiPrefix+"/login":
		return true
	case r.Method == http.MethodPost && r.URL.Path == apiPrefix+"/logout":
		return true
	case r.Method == http.MethodGet && r.URL.Path == apiPrefix+"/me":
		return true
	}
	return false
}

// sessionFrom resolves the session carried by the request cookie, if any.
func (s *Server) sessionFrom(r *http.Request) (*auth.Session, bool) {
	if s.sessions == nil {
		return nil, false
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil, false
	}
	sess := s.sessions.Lookup(c.Value)
	if sess == nil {
		return nil, false
	}
	return sess, true
}

// bearerTokenValue reads a bearer credential from the Authorization header,
// falling back to the webhook header only when no Authorization header is
// present.
func bearerTokenValue(r *http.Request) (string, bool) {
	value := r.Header.Get("Authorization")
	if token := r.Header.Get("X-Strangle-Token"); token != "" && value == "" {
		value = "Bearer " + token
	}
	token, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || token == "" {
		return "", false
	}
	return token, true
}

// bearerTokenOK reports whether r carries the static configured API token.
func (s *Server) bearerTokenOK(r *http.Request) bool {
	if s.authToken == "" {
		return false
	}
	token, ok := bearerTokenValue(r)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) == 1
}

// tlsFiles validates the TLS option. Requiring both paths avoids silently
// falling back to plain HTTP when only half of a keypair is configured.
func (s *Server) tlsFiles() (string, string, error) {
	if (s.tlsCertFile == "") != (s.tlsKeyFile == "") {
		return "", "", errors.New("tls: cert_file and key_file must be set together")
	}
	return s.tlsCertFile, s.tlsKeyFile, nil
}

// ListenAndServe runs the HTTP server until ctx is canceled, then gracefully
// shuts it down. With WithTLS configured it serves HTTPS instead of HTTP.
func (s *Server) ListenAndServe(ctx context.Context) error {
	certFile, keyFile, err := s.tlsFiles()
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           s.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// The goroutine wakes when ctx is done, at which point ctx can no longer
	// be used, so the shutdown derives its own deadline from Background.
	go func() { //nolint:gosec // G118: ctx is canceled by the time the shutdown runs
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if certFile != "" {
		err = srv.ListenAndServeTLS(certFile, keyFile)
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// spaHandler serves static files from fsys, falling back to index.html
// so client-side routing works.
func spaHandler(fsys fs.FS) http.Handler {
	files := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if name == "" {
			name = "."
		}
		if f, err := fsys.Open(name); err == nil {
			defer f.Close() //nolint:errcheck // best-effort close of the probe file
			if st, err := f.Stat(); err == nil && !st.IsDir() {
				files.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFileFS(w, r, fsys, "index.html")
	})
}

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
