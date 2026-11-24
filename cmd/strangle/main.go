// Command strangle applies Ansible playbooks on a schedule and reports on
// the results through an HTTP API and web UI.
//
// The master process (the root command) spawns worker child processes that
// each own one embedded Ansible interpreter and dispatch playbook runs to
// them over unix sockets (the "worker" subcommand).
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
	"github.com/kakwa/strangle/internal/collection"
	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/riverqueue"
	"github.com/kakwa/strangle/internal/scheduler"
	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/sshpool"
	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/internal/worker"
	"github.com/kakwa/strangle/proto/strangle/v1"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
)

var (
	serverConfigPath     string
	collectionConfigPath string
	socketPath           string
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

	root.AddCommand(workerCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// workerCmd is the subcommand run inside a worker child process: it serves
// the Runner gRPC service over a unix socket.
func workerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run as a worker child process (started by the master, not for direct use).",
		RunE:  runWorker,
	}
	cmd.Flags().StringVar(&socketPath, "socket", "", "unix socket path to serve the Runner gRPC service on (required)")
	cmd.Flags().StringVarP(&serverConfigPath, "server-config", "s", "config/server.yml", "path to the server configuration file")
	cmd.Flags().StringVarP(&collectionConfigPath, "collection-config", "c", "config/collection.yml", "path to the collection configuration file")
	_ = cmd.MarkFlagRequired("socket")
	return cmd
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// Git-backed targets are cloned into their configured path and pinned
	// to their tracked ref. A failure is reported, not fatal: the target
	// can be synced later through POST /api/sync.
	collMgr := collection.New(cfg.Targets, logger)
	if collMgr.HasGit() {
		if err := collMgr.InitialSync(ctx); err != nil {
			logger.Error("collection sync failed at startup", zap.Error(err))
		}
	}

	// The master keeps its own SSH multi for the host-key approval API; the
	// trust decisions are shared with the workers through the per-scope
	// files on disk.
	multi, err := sshpool.BuildMulti(collMgr.Targets())
	if err != nil {
		return fmt.Errorf("failed to initialize SSH transport: %w", err)
	}
	defer multi.Close()

	// Spawn the worker child processes that actually run the playbooks.
	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	mgr, err := worker.NewManager(ctx, binPath, serverConfigPath, collectionConfigPath, cfg.Server.Workers, logger)
	if err != nil {
		return fmt.Errorf("failed to start workers: %w", err)
	}
	// Supervise the pool: a worker that dies or stops answering health
	// pings is restarted (with backoff) by the manager.
	mgr.Start(ctx)
	go func() {
		<-mgr.Down()
		logger.Error("a worker slot is permanently down; playbook runs will fail until the master is restarted")
	}()

	var st store.Store
	if cfg.Server.Database.DSN != "" {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		d, err := db.Open(dctx, cfg.Server.Database.DSN)
		cancel()
		if err != nil {
			_ = mgr.Stop()
			return fmt.Errorf("failed to open database: %w", err)
		}
		st = d
		logger.Info("using PostgreSQL store")
	} else {
		st = store.NewMem()
		logger.Warn("no database DSN configured, using in-memory store")
	}

	apr := applier.NewAnsible(collMgr.Targets(), mgr, st, logger)

	// With a database, playbook runs flow through a postgres-backed River
	// queue: each cycle enqueues one job per (target, playbook) pair and a
	// River client in this process consumes them and dispatches runs to the
	// worker pool. Without a database, playbooks run directly.
	var queue *riverqueue.Client
	var enqueue server.RunEnqueuer
	var enqueueCycle func(ctx context.Context) error
	if cfg.Server.Database.DSN != "" {
		var err error
		queue, err = riverqueue.Open(ctx, cfg.Server.Database.DSN, cfg.Server.Workers, apr.Trigger, logger)
		if err != nil {
			_ = mgr.Stop()
			return fmt.Errorf("failed to initialize job queue: %w", err)
		}
		enqueue = queue
		enqueueCycle = func(ctx context.Context) error {
			// Read the live target list: git-backed targets gain their
			// playbooks only after a sync.
			var firstErr error
			for _, t := range collMgr.Targets() {
				for _, pb := range t.Playbooks {
					if _, err := queue.EnqueueRun(ctx, t.Name, pb.Name); err != nil {
						logger.Error("failed to enqueue playbook",
							zap.String("target", t.Name),
							zap.String("playbook", pb.Name),
							zap.Error(err),
						)
						if firstErr == nil {
							firstErr = err
						}
					}
				}
			}
			return firstErr
		}
	} else {
		enqueue = directEnqueuer{apr: apr, logger: logger}
		enqueueCycle = apr.RunCycle
	}

	sched := scheduler.New(cfg.Server.Interval, enqueueCycle, logger)
	var syncer server.Syncer
	if collMgr.HasGit() {
		syncer = collMgr
	}
	srv := server.New(cfg.Server.ListenAddr, apr, st, multi, enqueue, mgr, syncer, cfg.Server.Auth.Token, logger)

	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	if queue != nil {
		// The queue runs on its own context, not the signal context: a
		// SIGTERM must not hard-cancel in-flight runs; Stop drains them.
		queueCtx, cancelQueue := context.WithCancel(context.Background())
		defer cancelQueue()
		if err := queue.Start(queueCtx); err != nil {
			queue.Close()
			_ = mgr.Stop()
			return fmt.Errorf("failed to start job queue: %w", err)
		}
	}

	logger.Info("starting strangle",
		zap.String("addr", cfg.Server.ListenAddr),
		zap.Duration("interval", cfg.Server.Interval),
		zap.Int("workers", cfg.Server.Workers),
		zap.String("queue", queueMode(queue)),
		zap.Strings("targets", apr.TargetNames()),
	)
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server failed", zap.Error(err))
		shutdown(stop, done, mgr, st, queue, logger)
		return err
	}
	shutdown(stop, done, mgr, st, queue, logger)
	logger.Info("strangle stopped")
	return nil
}

// directEnqueuer runs a playbook in a background goroutine. It is used when
// no database is configured and there is no job queue to back manual runs.
type directEnqueuer struct {
	apr    *applier.Ansible
	logger *zap.Logger
}

func (d directEnqueuer) EnqueueRun(_ context.Context, target, playbook string) (string, error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		if err := d.apr.Trigger(ctx, target, playbook); err != nil {
			d.logger.Error("manual run failed",
				zap.String("target", target),
				zap.String("playbook", playbook),
				zap.Error(err),
			)
		}
	}()
	return "started", nil
}

func queueMode(queue *riverqueue.Client) string {
	if queue != nil {
		return "river"
	}
	return "direct"
}

// runWorker serves the Runner gRPC service inside a worker child process.
func runWorker(cmd *cobra.Command, args []string) error {
	if socketPath == "" {
		return errors.New("--socket is required")
	}

	cfg, err := config.Load(serverConfigPath, collectionConfigPath)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	logger, err := newLogger(cfg.Server.LogLevel)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	// The worker shares the master's checkouts: resolve the playbooks of
	// git-backed targets from disk so its SSH managers match what the
	// master will dispatch. Reload does not touch git.
	collMgr := collection.New(cfg.Targets, logger)
	if err := collMgr.Reload(); err != nil {
		logger.Error("failed to fully resolve collection for worker", zap.Error(err))
	}

	multi, err := sshpool.BuildMulti(collMgr.Targets())
	if err != nil {
		return fmt.Errorf("failed to initialize SSH transport: %w", err)
	}
	defer multi.Close()

	runner, err := ansible.NewRunner(multi)
	if err != nil {
		return fmt.Errorf("failed to initialize embedded Python: %w", err)
	}
	defer runner.Close()

	srv := worker.NewServer(runner, multi)
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(worker.MaxRecvBytes))
	v1.RegisterRunnerServer(grpcServer, srv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("starting worker", zap.Int("pid", os.Getpid()), zap.String("socket", socketPath))
	return worker.Run(ctx, socketPath, grpcServer, logger, 10*time.Second)
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

// shutdown stops the scheduler, drains the job queue (waiting for in-flight
// playbook runs to finish and be recorded), then terminates the workers and
// closes the store in that order: in-progress runs may still be writing to
// the store while they finish, and they run inside the worker processes.
func shutdown(stop context.CancelFunc, done <-chan struct{}, mgr *worker.Manager, st store.Store, queue *riverqueue.Client, logger *zap.Logger) {
	stop()
	<-done
	if queue != nil {
		qctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		if err := queue.Stop(qctx); err != nil {
			logger.Warn("job queue did not drain in time", zap.Error(err))
		}
		cancel()
		queue.Close()
	}
	if err := mgr.Stop(); err != nil {
		logger.Warn("failed to stop workers", zap.Error(err))
	}
	if err := st.Close(); err != nil {
		logger.Warn("failed to close store", zap.Error(err))
	}
}
