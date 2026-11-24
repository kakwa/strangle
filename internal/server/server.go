// Package server exposes the HTTP API and serves the web UI.
package server

import (
	"context"
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
	"github.com/kakwa/strangle/internal/sshpool"
	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/web"
	"go.uber.org/zap"
)

// Server is the strangle HTTP server.
type Server struct {
	addr    string
	applier *applier.Ansible
	store   store.Store
	ssh     *sshpool.Multi
	logger  *zap.Logger
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
// target uses the Go-managed SSH transport.
func New(addr string, apr *applier.Ansible, st store.Store, ssh *sshpool.Multi, logger *zap.Logger) *Server {
	return &Server{
		addr:    addr,
		applier: apr,
		store:   st,
		ssh:     ssh,
		logger:  logger,
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

	// /api/playbooks maps target name to its playbook list.
	mux.HandleFunc("GET /api/playbooks", func(w http.ResponseWriter, _ *http.Request) {
		out := map[string][]string{}
		for _, target := range s.applier.TargetNames() {
			out[target] = s.applier.Playbooks(target)
		}
		writeJSON(w, http.StatusOK, out)
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
		// The run happens in the background; the UI sees it appear in
		// /api/runs when it finishes (runs are serialized anyway).
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := s.applier.Trigger(ctx, req.Target, req.Playbook); err != nil {
				s.logger.Error("manual run failed",
					zap.String("target", req.Target),
					zap.String("playbook", req.Playbook),
					zap.Error(err),
				)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":   "started",
			"target":   req.Target,
			"playbook": req.Playbook,
		})
	})

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
