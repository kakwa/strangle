// Command strangle is a server that applies to job postings periodically
// and reports on the results.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/scheduler"
	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/store"
)

// jobs is the current set of job postings to apply to.
// TODO: load from a config file or database instead of hardcoding.
var jobs = []applier.Job{
	{ID: "j-001", URL: "https://example.com/jobs/1", Title: "Staff Engineer", Company: "Example Corp"},
	{ID: "j-002", URL: "https://example.com/jobs/2", Title: "Backend Developer", Company: "Acme Inc"},
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg := config.Load()
	st := store.New(100)
	apr := applier.NewStub(logger)

	sched := scheduler.New(cfg.Interval, jobs, apr, st, logger)
	srv := server.New(cfg.ListenAddr, jobs, st, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go sched.Run(ctx)

	logger.Info("starting strangle", "addr", cfg.ListenAddr, "interval", cfg.Interval)
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		os.Exit(1)
	}
	logger.Info("strangle stopped")
}
