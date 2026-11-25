// Package server exposes the HTTP API and serves the web UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/cfg"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/server/handlers"
	"github.com/kakwa/strangle/internal/sshpool"
	"github.com/kakwa/strangle/internal/worker"
	"github.com/kakwa/strangle/web"
	"github.com/labstack/echo/v5"
	echoSwagger "github.com/swaggo/echo-swagger/v2"
	swag "github.com/swaggo/swag/v2"
	"go.uber.org/zap"

	// Register the swag-generated OpenAPI document via its init().
	_ "github.com/kakwa/strangle/docs"
)

const (
	jsonMedia = "application/json"
	htmlMedia = "text/html"
	// SessionCookie is the name of the cookie carrying a UI session token.
	SessionCookie = handlers.SessionCookie
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
	store       db.Store
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
// (see package web), rooted at web/dist.
var uiFS = func() fs.FS {
	ui, err := fs.Sub(web.FS, "dist")
	if err != nil {
		// dist is embedded at compile time and always present.
		panic(err)
	}
	return ui
}()

// New returns a Server. The web UI is embedded in the binary; build it with
// `make web` (or `make build`) before compiling. ssh may be nil when no
// target uses the Go-managed SSH transport. workers may be nil when no worker
// pool is managed. syncer may be nil when no target has a git upstream.
// users, when non-nil, is the store of UI accounts; UI authentication is
// enforced as soon as it holds at least one user (see Routes). tokens, when
// non-nil, is the store of collection-scoped user API tokens. authToken, when
// non-empty, is a static bearer token accepted on the collection hook only.
// The run argument is kept for source compatibility and ignored.
func New(addr string, apr *applier.Ansible, st db.Store, ssh *sshpool.Multi, _ RunEnqueuer, workers WorkerReporter, syncer Syncer, users auth.UserStore, tokens auth.TokenStore, authToken string, logger *zap.Logger, opts ...Option) *Server {
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

// echoRouterPath converts a net/http-style pattern parameter ({name}) to the
// Echo v5 syntax (:name). Swagger annotations keep the standard OpenAPI
// braces; only the router uses colons.
var echoPathParam = regexp.MustCompile(`\{([^}]+)\}`)

func echoRouterPath(path string) string {
	return echoPathParam.ReplaceAllString(path, ":$1")
}

// echoRegistrar adapts Echo's router to the small registration interface used
// by the API handlers. Handlers keep their http.HandlerFunc shape and are
// wrapped with echo.WrapHandler, which also copies Echo path parameters into
// the request so r.PathValue keeps working.
type echoRegistrar struct {
	e *echo.Echo
}

func (r echoRegistrar) HandleFunc(pattern string, h http.HandlerFunc) {
	method, routePath, ok := strings.Cut(pattern, " ")
	if !ok || method == "" || routePath == "" {
		panic("server: invalid route pattern " + pattern)
	}
	r.e.Add(method, echoRouterPath(routePath), echo.WrapHandler(h))
}

// buildRoutes registers every route on an Echo instance and returns it. Echo
// implements http.Handler, so tests and the net/http server can use it
// directly.
func (s *Server) buildRoutes() *echo.Echo {
	s.initAuth()

	e := echo.New()
	e.HTTPErrorHandler = func(c *echo.Context, err error) {
		code := http.StatusInternalServerError
		if httpErr, ok := err.(*echo.HTTPError); ok {
			code = httpErr.Code
		}
		writeError(c.Response(), code, err)
	}
	api := s.api()
	e.Use(s.authMiddleware, collectionAccessMiddleware(api))

	s.registerAPIRoutes(echoRegistrar{e: e}, api)

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

// registerDocsRoutes serves the swag-generated OpenAPI 3.1 document and the
// embedded Swagger UI. Both are public: the spec describes the routes but
// exposes no collection data itself.
func (s *Server) registerDocsRoutes(e *echo.Echo) {
	e.GET(apiPrefix+"/openapi.json", func(c *echo.Context) error {
		doc, err := swag.ReadDoc()
		if err != nil {
			writeError(c.Response(), http.StatusInternalServerError, err)
			return nil
		}
		c.Response().Header().Set("Content-Type", jsonMedia)
		_, err = c.Response().Write([]byte(doc))
		return err
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonMedia)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
