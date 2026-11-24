// Command strangle applies Ansible playbooks on a schedule and reports on
// the results through an HTTP API and web UI.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kakwa/strangle/internal/ansible"
	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/scheduler"
	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/sshpool"
	"github.com/kakwa/strangle/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	sshManager, err := sshpool.New(cfg.SSH)
	if err != nil {
		logger.Error("failed to initialize SSH transport", "error", err)
		os.Exit(1)
	}
	if sshManager != nil {
		defer sshManager.Close()
	}

	runner, err := ansible.NewRunner(sshManager)
	if err != nil {
		logger.Error("failed to initialize embedded Python", "error", err)
		os.Exit(1)
	}

	var st store.Store
	if cfg.Database.DSN != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		d, err := db.Open(ctx, cfg.Database.DSN)
		cancel()
		if err != nil {
			logger.Error("failed to open database", "error", err)
			runner.Close()
			os.Exit(1)
		}
		st = d
		logger.Info("using PostgreSQL store")
	} else {
		st = store.NewMem()
		logger.Warn("no database DSN configured, using in-memory store")
	}

	apr := applier.NewAnsible(cfg.Ansible, runner, st, logger)
	sched := scheduler.New(cfg.Interval, apr.RunCycle, logger)
	srv := server.New(cfg.ListenAddr, apr, st, sshManager, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	logger.Info("starting strangle",
		"addr", cfg.ListenAddr,
		"interval", cfg.Interval,
		"playbooks", apr.Playbooks(),
		"working_dir", cfg.Ansible.WorkingDir,
	)
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", "error", err)
		shutdown(stop, done, runner, st, logger)
		os.Exit(1)
	}
	shutdown(stop, done, runner, st, logger)
	logger.Info("strangle stopped")
}

// shutdown stops the scheduler (waiting for any in-progress playbook run and
// its persistence to drain), then closes the runner and the store in that
// order: the runner must be closed first because an in-progress run may still
// be writing to the store while it finishes.
func shutdown(stop context.CancelFunc, done <-chan struct{}, runner *ansible.Runner, st store.Store, logger *slog.Logger) {
	stop()
	<-done
	runner.Close()
	if err := st.Close(); err != nil {
		logger.Warn("failed to close store", "error", err)
	}
}
