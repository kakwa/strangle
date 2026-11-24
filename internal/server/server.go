// Package server exposes the HTTP API and serves the web UI.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/collection"
	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/sshpool"
	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/internal/worker"
	"github.com/kakwa/strangle/web"
	"go.uber.org/zap"
)

// RunEnqueuer accepts a request to run one playbook now. In production this
// enqueues a river job; without a database it runs the playbook directly.
// It returns a short status word for the API response ("queued" for a
// background job, "started" for a direct run).
type RunEnqueuer interface {
	EnqueueRun(ctx context.Context, target, playbook string) (string, error)
}

// WorkerReporter reports the state of the master's worker pool. It is nil
// when no workers are managed.
type WorkerReporter interface {
	WorkerStatuses(ctx context.Context) []worker.WorkerStatus
}

// Syncer syncs a git-backed collection target: moves its checkout to the
// tracked ref and reloads its strangle.yml. It is nil when no target has a
// git upstream.
type Syncer interface {
	Sync(ctx context.Context, target, branch string) (string, bool, error)
	// Targets reports the resolved target configs, so the applier's view
	// can be refreshed after a sync.
	Targets() []config.TargetConfig
}

// Server is the strangle HTTP server.
type Server struct {
	addr      string
	applier   *applier.Ansible
	store     store.Store
	ssh       *sshpool.Multi
	run       RunEnqueuer
	workers   WorkerReporter
	syncer    Syncer
	authToken string
	logger    *zap.Logger
}

// playbookInfo describes one configured job with its latest run outcome.
type playbookInfo struct {
	Name       string     `json:"name"`
	Playbook   string     `json:"playbook"`
	Inventory  string     `json:"inventory"`
	LastStatus string     `json:"last_status,omitempty"` // ok | failed | ""
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
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
// target uses the Go-managed SSH transport. run accepts manual playbook runs
// (a river job in production, a direct run without a database). workers may
// be nil when no worker pool is managed. syncer may be nil when no target
// has a git upstream. authToken, when non-empty, is required as a bearer
// token on the sync endpoint (webhook protection).
func New(addr string, apr *applier.Ansible, st store.Store, ssh *sshpool.Multi, run RunEnqueuer, workers WorkerReporter, syncer Syncer, authToken string, logger *zap.Logger) *Server {
	return &Server{
		addr:      addr,
		applier:   apr,
		store:     st,
		ssh:       ssh,
		run:       run,
		workers:   workers,
		syncer:    syncer,
		authToken: authToken,
		logger:    logger,
	}
}

// Routes builds the HTTP handler: the /api endpoints plus the web UI.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		stats, err := s.store.Stats(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, stats)
	})

	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		limit := intQuery(r, "limit", 50)
		runs, err := s.store.ListRuns(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, runs)
	})

	mux.HandleFunc("GET /api/hosts", func(w http.ResponseWriter, r *http.Request) {
		hosts, err := s.store.ListHosts(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, hosts)
	})

	mux.HandleFunc("GET /api/hosts/{host}/runs", func(w http.ResponseWriter, r *http.Request) {
		limit := intQuery(r, "limit", 10)
		runs, err := s.store.HostRuns(r.Context(), r.PathValue("host"), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, runs)
	})

	mux.HandleFunc("GET /api/hosts/{host}/changes", func(w http.ResponseWriter, r *http.Request) {
		limit := intQuery(r, "limit", 200)
		changes, err := s.store.HostChanges(r.Context(), r.PathValue("host"), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, changes)
	})

	// /api/playbooks maps each target to its configured jobs, with the
	// latest run outcome of each job.
	mux.HandleFunc("GET /api/playbooks", func(w http.ResponseWriter, r *http.Request) {
		runs, err := s.store.ListRuns(r.Context(), 1000)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		latest := map[string]store.RunRecord{}
		for _, run := range runs {
			key := run.Target + "\x00" + run.Playbook
			if cur, ok := latest[key]; !ok || run.FinishedAt.After(cur.FinishedAt) {
				latest[key] = run
			}
		}

		out := map[string][]playbookInfo{}
		for _, target := range s.applier.TargetNames() {
			configs := s.applier.PlaybookConfigs(target)
			pbs := make([]playbookInfo, 0, len(configs))
			for _, pb := range configs {
				info := playbookInfo{
					Name:      pb.Name,
					Playbook:  strings.TrimPrefix(pb.Playbook, "./"),
					Inventory: strings.TrimPrefix(pb.Inventory, "./"),
				}
				if run, ok := latest[target+"\x00"+pb.Name]; ok {
					info.LastStatus = map[bool]string{true: "ok", false: "failed"}[run.Success]
					at := run.FinishedAt
					info.LastRunAt = &at
				}
				pbs = append(pbs, info)
			}
			out[target] = pbs
		}
		writeJSON(w, http.StatusOK, out)
	})

	// /api/workers reports the master's worker pool: one entry per worker
	// process, with liveness probed over gRPC.
	mux.HandleFunc("GET /api/workers", func(w http.ResponseWriter, r *http.Request) {
		if s.workers == nil {
			writeJSON(w, http.StatusOK, []worker.WorkerStatus{})
			return
		}
		writeJSON(w, http.StatusOK, s.workers.WorkerStatuses(r.Context()))
	})

	mux.HandleFunc("POST /api/run", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target   string `json:"target"`
			Playbook string `json:"playbook"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Target == "" {
			writeError(w, http.StatusBadRequest, errors.New("target is required"))
			return
		}
		if req.Playbook == "" {
			writeError(w, http.StatusBadRequest, errors.New("playbook is required"))
			return
		}
		if !s.applier.KnownPlaybook(req.Target, req.Playbook) {
			writeError(w, http.StatusNotFound, fmt.Errorf("unknown playbook %q for target %q", req.Playbook, req.Target))
			return
		}
		// req.Playbook is the job name from strangle.yml, not a file path.
		// The run happens in the background (a river job, or a direct run
		// without a database); the UI sees it appear in /api/runs when it
		// finishes.
		status, err := s.run.EnqueueRun(r.Context(), req.Target, req.Playbook)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":   status,
			"target":   req.Target,
			"playbook": req.Playbook,
		})
	})

	// /api/sync is the webhook for git-backed collections: it moves the
	// target's checkout to its tracked ref (or to the given branch, which
	// becomes the new tracked ref) and reloads its strangle.yml. When
	// auth.token is set in server.yml, a matching bearer token is required.
	mux.HandleFunc("POST /api/sync", func(w http.ResponseWriter, r *http.Request) {
		if s.syncer == nil {
			writeError(w, http.StatusNotFound, errors.New("no git-backed collection is configured"))
			return
		}
		if s.authToken != "" && !s.bearerTokenOK(r) {
			writeError(w, http.StatusUnauthorized, errors.New("missing or invalid bearer token"))
			return
		}
		var req struct {
			Target string `json:"target"`
			Branch string `json:"branch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if req.Target == "" {
			writeError(w, http.StatusBadRequest, errors.New("target is required"))
			return
		}
		rev, changed, err := s.syncer.Sync(r.Context(), req.Target, req.Branch)
		if err != nil {
			switch {
			case errors.Is(err, collection.ErrUnknownTarget):
				writeError(w, http.StatusNotFound, err)
			case errors.Is(err, collection.ErrNotGit):
				writeError(w, http.StatusBadRequest, err)
			default:
				// A git failure (network, auth, broken strangle.yml).
				writeError(w, http.StatusBadGateway, err)
			}
			return
		}
		// The sync may have changed the target's playbooks (a branch
		// switch, a new strangle.yml); refresh the applier's view.
		s.applier.UpdateTargets(s.syncer.Targets())
		writeJSON(w, http.StatusOK, map[string]any{
			"target":  req.Target,
			"rev":     rev,
			"changed": changed,
		})
	})

	// Host-key endpoints identify an SSH scope as "<target>/<playbook name>"
	// in their `target` field (or the JSON response's `target` key).
	mux.HandleFunc("GET /api/hostkeys", func(w http.ResponseWriter, _ *http.Request) {
		keys := []sshpool.HostKey{}
		if s.ssh != nil {
			keys = s.ssh.PendingHostKeys()
		}
		writeJSON(w, http.StatusOK, keys)
	})

	mux.HandleFunc("GET /api/ssh/stats", func(w http.ResponseWriter, _ *http.Request) {
		stats := map[string]sshpool.Stats{}
		if s.ssh != nil {
			stats = s.ssh.Stats()
		}
		writeJSON(w, http.StatusOK, stats)
	})

	mux.HandleFunc("POST /api/hostkeys/accept", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target      string `json:"target"`
			Host        string `json:"host"`
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if s.ssh == nil {
			writeError(w, http.StatusBadRequest, errors.New("SSH host key approval is not enabled"))
			return
		}
		if err := s.ssh.ApproveHostKey(req.Target, req.Host, req.Fingerprint); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "accepted", "host": req.Host})
	})

	mux.HandleFunc("POST /api/hostkeys/reject", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Target string `json:"target"`
			Host   string `json:"host"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if s.ssh == nil {
			writeError(w, http.StatusBadRequest, errors.New("SSH host key approval is not enabled"))
			return
		}
		if err := s.ssh.RejectHostKey(req.Target, req.Host); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "rejected", "host": req.Host})
	})

	mux.Handle("/", spaHandler(uiFS))

	return mux
}

// ListenAndServe runs the HTTP server until ctx is canceled,
// then gracefully shuts it down.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{Addr: s.addr, Handler: s.Routes()}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	err := srv.ListenAndServe()
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

// bearerTokenOK reports whether r carries the configured API token, either
// as an Authorization: Bearer header or as an X-Strangle-Token header (the
// latter is friendlier for webhook integrations).
func (s *Server) bearerTokenOK(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if token := r.Header.Get("X-Strangle-Token"); token != "" && auth == "" {
		auth = "Bearer " + token
	}
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) == 1
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
