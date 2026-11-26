// Command strangle-server runs the strangle master process and its workers.
//
// "strangle-server serve" runs the master process, which spawns worker child
// processes that each own one embedded Ansible interpreter and dispatch
// playbook runs to them over unix sockets (the "worker" subcommand).
// Operator-facing commands live in the separate strangle-cli binary.
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

	"github.com/kakwa/strangle/api"
	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/controller/collection"
	"github.com/kakwa/strangle/controller/maintenance"
	"github.com/kakwa/strangle/controller/scheduler"
	"github.com/kakwa/strangle/controller/worker"
	"github.com/kakwa/strangle/controller/worker/riverqueue"
	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/security/auth"
	"github.com/kakwa/strangle/security/sandbox"
	"github.com/kakwa/strangle/utils/cfg"
	"github.com/kakwa/strangle/utils/logger"
	"github.com/kakwa/strangle/utils/sshpool"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var (
	serverConfigPath     string
	collectionConfigPath string
	socketPath           string
	sandboxManifestPath  string
)

func main() {
	root := &cobra.Command{
		Use:   "strangle-server",
		Short: "Run the strangle server and its workers.",
		// The error is printed by main; keep cobra from printing it too.
		SilenceErrors: true,
		SilenceUsage:  true,
		Long: "strangle-server applies Ansible playbooks from a collection of targets " +
			"on a schedule and reports on the results through an HTTP API and web UI.\n\n" +
			"It reads two configuration files: a server file with process-level " +
			"settings and a collection file listing the Ansible targets to apply. " +
			"Each target directory must contain its own strangle.yml.\n\n" +
			"Run \"strangle-server serve\" to start the master process. Operator " +
			"commands (user, token, collection, run, hosts, ...) are in strangle-cli.",
	}

	root.AddCommand(serveCmd())
	root.AddCommand(workerCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// serveCmd starts the master process: it spawns the worker child processes,
// schedules the playbook runs and serves the HTTP API and web UI.
func serveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the strangle server (master process).",
		RunE:  run,
	}
	cmd.Flags().StringVarP(&serverConfigPath, "server-config", "s", "config/api.yml", "path to the server configuration file")
	cmd.Flags().StringVarP(&collectionConfigPath, "collection-config", "c", "config/collection.yml", "path to the collection configuration file")
	return cmd
}

// workerCmd is the subcommand run inside a worker child process: it serves
// the worker JSON-RPC API over a unix socket.
func workerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run as a worker child process (started by the master, not for direct use).",
		RunE:  runWorker,
	}
	cmd.Flags().StringVar(&socketPath, "socket", "", "unix socket path to serve the worker JSON-RPC API on (required)")
	cmd.Flags().StringVarP(&serverConfigPath, "server-config", "s", "config/api.yml", "path to the server configuration file")
	cmd.Flags().StringVarP(&collectionConfigPath, "collection-config", "c", "config/collection.yml", "path to the collection configuration file")
	cmd.Flags().StringVar(&sandboxManifestPath, "sandbox", "", "Landlock manifest written by the master (internal use)")
	_ = cmd.MarkFlagRequired("socket")
	return cmd
}

func run(cmd *cobra.Command, args []string) error {
	conf, err := cfg.Load(serverConfigPath, collectionConfigPath)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	lg, err := logger.New(conf.Server.LogLevel)
	if err != nil {
		return err
	}
	defer func() { _ = lg.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// Git-backed targets are cloned into their configured path and pinned
	// to their tracked ref. A failure is reported, not fatal: the target
	// can be synced later through the collection hook endpoint.
	collMgr := collection.New(conf.Targets, lg)
	if collMgr.HasGit() {
		if err := collMgr.InitialSync(ctx); err != nil {
			lg.Error("collection sync failed at startup", zap.Error(err))
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
	workerOpts := []worker.ManagerOption{}
	if conf.Server.Sandbox.Enabled {
		if !sandbox.KernelSupported() {
			lg.Warn("Landlock is not supported by this kernel: collection sandboxing will not be enforced")
		}
		manifests, err := sandbox.Build(conf.Server.Sandbox, collMgr.Targets(), serverConfigPath, collectionConfigPath)
		if err != nil {
			return fmt.Errorf("build sandbox policies: %w", err)
		}
		workerOpts = append(workerOpts, worker.WithSandbox(manifests))
	}
	mgr, err := worker.NewManager(ctx, binPath, serverConfigPath, collectionConfigPath, conf.Server.Workers, lg, workerOpts...)
	if err != nil {
		return fmt.Errorf("failed to start workers: %w", err)
	}
	// Supervise the pool: a worker that dies or stops answering health
	// pings is restarted (with backoff) by the manager.
	mgr.Start(ctx)
	go func() {
		<-mgr.Down()
		lg.Error("a worker slot is permanently down; playbook runs will fail until the master is restarted")
	}()

	var (
		d     *model.DB
		queue *riverqueue.Client
	)
	// fail stops everything started so far, in reverse order, before
	// returning err.
	fail := func(format string, err error) error {
		if queue != nil {
			queue.Close()
		}
		if d != nil {
			warnOn(lg, "failed to close store", d.Close())
		}
		warnOn(lg, "failed to stop workers", mgr.Stop())
		return fmt.Errorf(format, err)
	}

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	d, err = model.Open(dctx, conf.Server.Database.DSN)
	cancel()
	if err != nil {
		return fail("failed to open database: %w", err)
	}

	// collection.yml (including any git checkout refreshed above) is the
	// source of truth for collections. Replace their database rows at startup
	// so IDs, options and playbooks match the live configuration.
	dctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	err = d.SyncCollections(dctx, collMgr.Targets())
	cancel()
	if err != nil {
		return fail("failed to load collections into database: %w", err)
	}

	var st model.Store = d
	var users auth.UserStore = d.Users()
	var tokens auth.TokenStore = d.Tokens()
	var access auth.CollectionAccess = d.Access()

	apr := applier.NewAnsible(collMgr.Targets(), mgr, st, lg)

	// Playbook runs flow through a postgres-backed River queue and a River
	// client in this process dispatches them to the worker pool. The
	// scheduler below decides when each individual (target, playbook) pair
	// runs.
	queue, err = riverqueue.Open(ctx, conf.Server.Database.DSN, conf.Server.Workers, func(ctx context.Context, target, playbook string, runID uint64, check bool) error {
		return apr.Trigger(applier.WithCheck(applier.WithRunID(ctx, runID), check), target, playbook)
	}, lg)
	if err != nil {
		return fail("failed to initialize job queue: %w", err)
	}
	var enqueue api.RunEnqueuer = queue

	// Read the live target list on every reconcile: git-backed targets gain
	// (or change) their playbooks and schedules only after a sync.
	jobProvider := func() []scheduler.Job {
		var jobs []scheduler.Job
		for _, t := range collMgr.Targets() {
			for _, pb := range t.Playbooks {
				jobs = append(jobs, scheduler.Job{
					Target:   t.Name,
					Playbook: pb.Name,
					Schedule: pb.Schedule,
				})
			}
		}
		return jobs
	}
	runScheduled := func(ctx context.Context, target, playbook string) error {
		record := model.RunRecord{Target: target, Playbook: playbook, State: model.RunStateQueued}
		if err := st.CreateRun(ctx, &record); err != nil {
			return err
		}
		jobID, err := enqueue.EnqueueRun(ctx, target, playbook, record.ID, false)
		if err != nil {
			return err
		}
		record.JobID = jobID
		if err := st.UpdateRun(ctx, record); err != nil {
			lg.Warn("failed to store job ID for scheduled run", zap.Uint64("run", record.ID), zap.Error(err))
		}
		return nil
	}
	sched := scheduler.New(conf.Server.Schedule, jobProvider, runScheduled, lg)
	var syncer api.Syncer
	if collMgr.HasGit() {
		syncer = collMgr
	}
	srv := api.New(conf.Server.ListenAddr, apr, st, multi, enqueue, mgr, syncer, users, tokens, conf.Server.Auth.Token, lg,
		api.WithCollectionAccess(access),
		api.WithTLS(conf.Server.TLS.CertFile, conf.Server.TLS.KeyFile))

	done := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(done)
	}()

	retentionDone := maintenance.Start(ctx,
		maintenance.NewRetention(st, conf.Server.Retention, collMgr.Targets, lg))
	cleanupDone := maintenance.Start(ctx,
		maintenance.NewHostCleanup(st, conf.Server.HostCleanup, collMgr.Targets, lg))

	// The queue runs on its own context, not the signal context: a SIGTERM
	// must not hard-cancel in-flight runs; Stop drains them.
	queueCtx, cancelQueue := context.WithCancel(context.Background())
	defer cancelQueue()
	if err := queue.Start(queueCtx); err != nil {
		return fail("failed to start job queue: %w", err)
	}

	lg.Info("starting strangle",
		zap.String("addr", conf.Server.ListenAddr),
		zap.String("default_schedule", conf.Server.Schedule),
		zap.Int("scheduled_jobs", len(jobProvider())),
		zap.Int("workers", conf.Server.Workers),
		zap.String("queue", "river"),
		zap.Strings("targets", apr.TargetNames()),
	)
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		lg.Error("server failed", zap.Error(err))
		shutdown(stop, done, retentionDone, cleanupDone, mgr, st, queue, lg)
		return err
	}
	shutdown(stop, done, retentionDone, cleanupDone, mgr, st, queue, lg)
	lg.Info("strangle stopped")
	return nil
}

// runWorker serves the worker JSON-RPC API inside a worker child process.
func runWorker(cmd *cobra.Command, args []string) error {
	if socketPath == "" {
		return errors.New("--socket is required")
	}

	// A sandboxed worker is pinned to one collection. Read and apply its
	// manifest before any SSH manager or embedded interpreter is created,
	// so no sibling collection's credentials enter this process.
	var manifest sandbox.Manifest
	if sandboxManifestPath != "" {
		var err error
		manifest, err = sandbox.ReadFile(sandboxManifestPath)
		if err != nil {
			return err
		}
		sandbox.SetEnv(manifest.Env)
	}

	conf, err := cfg.Load(serverConfigPath, collectionConfigPath)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if manifest.Collection != "" {
		owned := make([]cfg.TargetConfig, 0, 1)
		for _, t := range conf.Targets {
			if t.Name == manifest.Collection {
				owned = append(owned, t)
			}
		}
		if len(owned) != 1 {
			return fmt.Errorf("sandbox manifest names unknown collection %q", manifest.Collection)
		}
		conf.Targets = owned
	}

	lg, err := logger.New(conf.Server.LogLevel)
	if err != nil {
		return err
	}
	defer func() { _ = lg.Sync() }()

	if sandboxManifestPath != "" {
		if !sandbox.KernelSupported() {
			lg.Warn("Landlock is not supported by this kernel: sandboxing is not enforced")
		}
		if err := sandbox.Apply(manifest); err != nil {
			return fmt.Errorf("apply sandbox: %w", err)
		}
	}

	// The worker shares the master's checkouts: resolve the playbooks of
	// git-backed targets from disk so its SSH managers match what the
	// master will dispatch. Reload does not touch git.
	collMgr := collection.New(conf.Targets, lg)
	if err := collMgr.Reload(); err != nil {
		lg.Error("failed to fully resolve collection for worker", zap.Error(err))
	}

	multi, err := sshpool.BuildMulti(collMgr.Targets())
	if err != nil {
		return fmt.Errorf("failed to initialize SSH transport: %w", err)
	}
	defer multi.Close()

	runner, err := applier.NewRunner(multi)
	if err != nil {
		return fmt.Errorf("failed to initialize embedded Python: %w", err)
	}
	defer runner.Close()

	srv := worker.NewRPCServer(worker.NewServer(runner, multi))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	lg.Info("starting worker", zap.Int("pid", os.Getpid()), zap.String("socket", socketPath))
	return worker.Serve(ctx, socketPath, srv, lg, 10*time.Second)
}

// shutdown stops the scheduler, drains the job queue (waiting for in-flight
// playbook runs to finish and be recorded), then terminates the workers and
// closes the store in that order: in-progress runs may still be writing to
// the store while they finish, and they run inside the worker processes.
func shutdown(stop context.CancelFunc, done, retentionDone, cleanupDone <-chan struct{}, mgr *worker.Manager, st model.Store, queue *riverqueue.Client, lg *zap.Logger) {
	stop()
	<-done
	<-retentionDone
	<-cleanupDone
	qctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	warnOn(lg, "job queue did not drain in time", queue.Stop(qctx))
	cancel()
	queue.Close()
	warnOn(lg, "failed to stop workers", mgr.Stop())
	warnOn(lg, "failed to close store", st.Close())
}

// warnOn logs what as a warning if err is non-nil.
func warnOn(lg *zap.Logger, what string, err error) {
	if err != nil {
		lg.Warn(what, zap.Error(err))
	}
}
