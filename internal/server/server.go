// Package server exposes the HTTP API and serves the web UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/sshpool"
	"github.com/kakwa/strangle/internal/store"
)

// Server is the strangle HTTP server.
type Server struct {
	addr       string
	jobs       []applier.Job
	store      *store.Store
	sshManager *sshpool.Manager
	logger     *slog.Logger
	uiDir      string
}

// New returns a Server. The web UI is served from uiDir
// (expected to be the built output in web/dist).
func New(addr string, jobs []applier.Job, st *store.Store, sshManager *sshpool.Manager, logger *slog.Logger) *Server {
	return &Server{
		addr:       addr,
		jobs:       jobs,
		store:      st,
		sshManager: sshManager,
		logger:     logger,
		uiDir:      filepath.Join("web", "dist"),
	}
}

// Routes builds the HTTP handler: the /api endpoints plus the web UI.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.jobs)
	})

	mux.HandleFunc("GET /api/reports", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.store.List())
	})

	mux.HandleFunc("GET /api/hostkeys", func(w http.ResponseWriter, _ *http.Request) {
		keys := []sshpool.HostKey{}
		if hostKeys := s.sshManager.HostKeys(); hostKeys != nil {
			keys = hostKeys.Pending()
		}
		writeJSON(w, http.StatusOK, keys)
	})

	mux.HandleFunc("GET /api/ssh/stats", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.sshManager.Stats())
	})

	mux.HandleFunc("POST /api/hostkeys/accept", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Host        string `json:"host"`
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		hostKeys := s.sshManager.HostKeys()
		if hostKeys == nil {
			writeError(w, http.StatusBadRequest, errors.New("SSH host key approval is not enabled"))
			return
		}
		if err := hostKeys.Approve(req.Host, req.Fingerprint); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "accepted", "host": req.Host})
	})

	mux.HandleFunc("POST /api/hostkeys/reject", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Host string `json:"host"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		hostKeys := s.sshManager.HostKeys()
		if hostKeys == nil {
			writeError(w, http.StatusBadRequest, errors.New("SSH host key approval is not enabled"))
			return
		}
		if err := hostKeys.Reject(req.Host); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "rejected", "host": req.Host})
	})

	mux.Handle("/", spaHandler(s.uiDir))

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

// spaHandler serves static files from dir, falling back to index.html
// so client-side routing works.
func spaHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(dir, filepath.Clean(strings.TrimPrefix(r.URL.Path, "/")))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			fs.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
