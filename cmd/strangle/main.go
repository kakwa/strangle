// Command strangle applies Ansible playbooks on a schedule and reports on
// the results through an HTTP API and web UI.
package main

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	serverConfigPath     string
	collectionConfigPath string
)

func main() {
	root := &cobra.Command{
		Use:   "strangle",
		Short: "Apply Ansible playbooks on a schedule and report the results.",
		// The error is printed by main; keep cobra from printing it too.
		SilenceErrors: true,
		SilenceUsage:  true,
		Long: "strangle applies Ansible playbooks from a collection of targets on a " +
			"schedule and reports on the results through an HTTP API and web UI.\n\n" +
			"It reads two configuration files: a server file with process-level " +
			"settings and a collection file listing the Ansible targets to apply. " +
			"Each target directory must contain its own strangle.yml.",
		RunE: run,
	}
	root.Flags().StringVarP(&serverConfigPath, "server-config", "s", "config/server.yml", "path to the server configuration file")
	root.Flags().StringVarP(&collectionConfigPath, "collection-config", "c", "config/collection.yml", "path to the collection configuration file")

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(serverConfigPath, collectionConfigPath)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	logger, err := newLogger(cfg.Server.LogLevel)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	multi, err := buildSSH(cfg.Targets)
	if err != nil {
		return fmt.Errorf("failed to initialize SSH transport: %w", err)
	}
	defer multi.Close()

	runner, err := ansible.NewRunner(multi)
	if err != nil {
		return fmt.Errorf("failed to initialize embedded Python: %w", err)
	}

	var st store.Store
	if cfg.Server.Database.DSN != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		d, err := db.Open(ctx, cfg.Server.Database.DSN)
		cancel()
		if err != nil {
			runner.Close()
			return fmt.Errorf("failed to open database: %w", err)
		}
		st = d
		logger.Info("using PostgreSQL store")
	} else {
		st = store.NewMem()
		logger.Warn("no database DSN configured, using in-memory store")
	}

	apr := applier.NewAnsible(cfg.Targets, runner, multi, st, logger)
	sched := scheduler.New(cfg.Server.Interval, apr.RunCycle, logger)
	srv := server.New(cfg.Server.ListenAddr, apr, st, multi, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	logger.Info("starting strangle",
		zap.String("addr", cfg.Server.ListenAddr),
		zap.Duration("interval", cfg.Server.Interval),
		zap.Strings("targets", apr.TargetNames()),
	)
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", zap.Error(err))
		shutdown(stop, done, runner, st, logger)
		return err
	}
	shutdown(stop, done, runner, st, logger)
	logger.Info("strangle stopped")
	return nil
}

// buildSSH creates one SSH manager per target that enables the Go-managed SSH
// transport, and a dispatcher over them. The result is nil when no target
// uses the transport.
func buildSSH(targets []config.TargetConfig) (*sshpool.Multi, error) {
	managers := map[string]*sshpool.Manager{}
	for _, t := range targets {
		if !t.SSH.Enabled {
			continue
		}
		mgr, err := sshpool.New(t.SSH)
		if err != nil {
			return nil, fmt.Errorf("target %q: %w", t.Name, err)
		}
		managers[t.Name] = mgr
	}
	return sshpool.NewMulti(managers)
}

// newLogger builds a JSON logger on stderr at the given level.
func newLogger(level string) (*zap.Logger, error) {
	var lvl zapcore.Level
	switch level {
	case "debug":
		lvl = zap.DebugLevel
	case "info":
		lvl = zap.InfoLevel
	case "warn":
		lvl = zap.WarnLevel
	case "error":
		lvl = zap.ErrorLevel
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	cfg.EncoderConfig.TimeKey = "ts"
	return cfg.Build()
}

// shutdown stops the scheduler (waiting for any in-progress playbook run and
// its persistence to drain), then closes the runner and the store in that
// order: the runner must be closed first because an in-progress run may still
// be writing to the store while it finishes.
func shutdown(stop context.CancelFunc, done <-chan struct{}, runner *ansible.Runner, st store.Store, logger *zap.Logger) {
	stop()
	<-done
	runner.Close()
	if err := st.Close(); err != nil {
		logger.Warn("failed to close store", zap.Error(err))
	}
}
