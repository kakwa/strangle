// Package api implements the versioned Strangle HTTP API: the handlers and
// their swaggo annotations, the route table, and the transport wiring
// (authentication middleware, metrics, docs, static UI) around them.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/controller/worker"
	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/security/auth"
	"github.com/kakwa/strangle/ui"
	"github.com/kakwa/strangle/utils/cfg"
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

// WorkerReporter reports the state of the master's worker pool. It is nil
// when no workers are managed; /metrics then reports zero worker slots.
type WorkerReporter interface {
	WorkerStatuses(ctx context.Context) []worker.WorkerStatus
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

// WithWorkerReporter overrides the worker-pool status reporter used by
// /metrics. Production passes the worker manager to New; tests can supply a
// canned reporter without starting real workers.
func WithWorkerReporter(w WorkerReporter) Option {
	return func(s *Server) { s.workers = w }
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
	workers     WorkerReporter
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
// target uses the Go-managed SSH transport. workers may be nil when no worker
// pool is managed. syncer may be nil when no target has a git upstream.
// users, when non-nil, is the store of UI accounts; UI authentication is
// enforced as soon as it holds at least one user (see Routes). tokens, when
// non-nil, is the store of collection-scoped user API tokens. authToken, when
// non-empty, is a static bearer token accepted on the collection hook only.
// The run argument is kept for source compatibility and ignored.
func New(addr string, apr *applier.Ansible, st model.Store, ssh *sshpool.Multi, _ RunEnqueuer, workers WorkerReporter, syncer Syncer, users auth.UserStore, tokens auth.TokenStore, authToken string, logger *zap.Logger, opts ...Option) *Server {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Server{
		addr:      addr,
		applier:   apr,
		store:     st,
		ssh:       ssh,
		workers:   workers,
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

// Routes builds the HTTP handler: the /api/v1 endpoints, /metrics, the API
// docs and the embedded web UI.
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

	// Prometheus scrape endpoint. It is intentionally outside /api so it is
	// reachable without a UI session, and reports worker and runtime state.
	e.GET("/metrics", echo.WrapHandler(metricsHandler(s.workers)))

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
	srv := &http.Server{Addr: s.addr, Handler: s.Routes()}

	go func() {
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
			if st, err := f.Stat(); err == nil && !st.IsDir() {
				f.Close()
				files.ServeHTTP(w, r)
				return
			}
			f.Close()
		}
		http.ServeFileFS(w, r, fsys, "index.html")
	})
}
