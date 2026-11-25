// Command strangle applies Ansible playbooks on a schedule and reports on
// the results through an HTTP API and web UI.
//
// "strangle serve" runs the master process, which spawns worker child
// processes that each own one embedded Ansible interpreter and dispatch
// playbook runs to them over unix sockets (the "worker" subcommand).
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kakwa/strangle/internal/ansible"
	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/collection"
	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/notify"
	"github.com/kakwa/strangle/internal/riverqueue"
	"github.com/kakwa/strangle/internal/sandbox"
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
	sandboxManifestPath  string
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
			"Each target directory must contain its own strangle.yml.\n\n" +
			"Run \"strangle serve\" to start the master process, or \"strangle " +
			"<command> --help\" for the other subcommands.",
	}

	root.AddCommand(serveCmd())
	root.AddCommand(workerCmd())
	root.AddCommand(userCmd())
	root.AddCommand(tokenCmd())
	root.AddCommand(collectionCmd())

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
	cmd.Flags().StringVarP(&serverConfigPath, "server-config", "s", "config/server.yml", "path to the server configuration file")
	cmd.Flags().StringVarP(&collectionConfigPath, "collection-config", "c", "config/collection.yml", "path to the collection configuration file")
	return cmd
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
	cmd.Flags().StringVar(&sandboxManifestPath, "sandbox", "", "Landlock manifest written by the master (internal use)")
	_ = cmd.MarkFlagRequired("socket")
	return cmd
}

// userCmd manages the local UI accounts stored in the database users table:
//
//	strangle user add <name> --password <pw>
//	strangle user add <name> --password-stdin
//	strangle user list
//	strangle user remove <name>
//
// Accounts are provisioned out of band, not through the API. Once the first
// account exists, the web UI requires a login.
func userCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage local UI accounts (stored in the database users table).",
	}
	// The accounts live in the configured database, so the subcommands need
	// the server file to find the DSN.
	cmd.PersistentFlags().StringVarP(&serverConfigPath, "server-config", "s",
		"config/server.yml", "path to the server configuration file")
	var password string
	var passwordStdin bool

	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a UI account.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var pw string
			switch {
			case passwordStdin:
				pw = readPasswordStdin()
			case password != "":
				pw = password
			default:
				return errors.New("provide the password with --password or --password-stdin")
			}
			if pw == "" {
				return errors.New("the password must not be empty")
			}
			hash, err := auth.HashPassword(pw)
			if err != nil {
				return err
			}
			users, err := openUserStore()
			if err != nil {
				return err
			}
			if err := users.CreateUser(context.Background(), args[0], hash); err != nil {
				return err
			}
			fmt.Printf("created user %q\n", args[0])
			return nil
		},
	}
	add.Flags().StringVar(&password, "password", "", "the account password (or use --password-stdin)")
	add.Flags().BoolVar(&passwordStdin, "password-stdin", false, "read the password from a line of stdin")

	list := &cobra.Command{
		Use:   "list",
		Short: "List UI accounts.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			users, err := openUserStore()
			if err != nil {
				return err
			}
			all, err := users.ListUsers(context.Background())
			if err != nil {
				return err
			}
			for _, u := range all {
				fmt.Println(u.Name)
			}
			return nil
		},
	}

	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete a UI account.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			users, err := openUserStore()
			if err != nil {
				return err
			}
			if err := users.RemoveUser(context.Background(), args[0]); err != nil {
				if errors.Is(err, auth.ErrNotFound) {
					return fmt.Errorf("user %q not found", args[0])
				}
				return err
			}
			fmt.Printf("removed user %q\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(add, list, remove)
	return cmd
}

// openUserStore opens the users table from the configured server file. UI
// accounts only exist when a database is configured.
func openUserStore() (auth.UserStore, error) {
	cfg, err := config.LoadServer(serverConfigPath)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	if cfg.Database.DSN == "" {
		return nil, errors.New("no database.dsn in server.yml: UI accounts are stored in the database")
	}
	d, err := db.Open(context.Background(), cfg.Database.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	return d.Users(), nil
}

// readPasswordStdin reads one line from stdin and strips the trailing
// newline (and carriage return).
func readPasswordStdin() string {
	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return ""
	}
	return strings.TrimRight(sc.Text(), "\r\n")
}

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens for user-linked CLIs and integrations (stored in the database).",
	}
	// The tokens live in the configured database, so the subcommands need
	// the server file to find the DSN.
	cmd.PersistentFlags().StringVarP(&serverConfigPath, "server-config", "s",
		"config/server.yml", "path to the server configuration file")

	var user string
	var ttl time.Duration

	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an API token for a user; the raw value is shown exactly once.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			users, tokens, err := openTokenStores()
			if err != nil {
				return err
			}
			if user == "" {
				return errors.New("provide the owning user with --user")
			}
			ctx := context.Background()
			if _, err := users.GetUser(ctx, user); err != nil {
				if errors.Is(err, auth.ErrNotFound) {
					return fmt.Errorf("user %q not found", user)
				}
				return err
			}
			raw, tok, err := tokens.Create(ctx, user, args[0], ttl)
			if err != nil {
				return err
			}
			// The raw value goes to stdout (so it can be captured with
			// $()); everything else goes to stderr.
			fmt.Println(raw)
			expiry := "never expires"
			if tok.Expires != nil {
				expiry = "expires " + tok.Expires.Format("2006-01-02 15:04:05")
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "created token %q for user %s (%s)\n", tok.Name, user, expiry)
			return nil
		},
	}
	create.Flags().StringVar(&user, "user", "", "the user the token belongs to (required)")
	create.Flags().DurationVar(&ttl, "ttl", 0, "token lifetime (e.g. 72h); 0 means no expiry")

	list := &cobra.Command{
		Use:   "list",
		Short: "List API tokens, for one user (--user) or for all users.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			users, tokens, err := openTokenStores()
			if err != nil {
				return err
			}
			ctx := context.Background()
			owners := []string{}
			switch {
			case user != "":
				if _, err := users.GetUser(ctx, user); err != nil {
					if errors.Is(err, auth.ErrNotFound) {
						return fmt.Errorf("user %q not found", user)
					}
					return err
				}
				owners = []string{user}
			default:
				all, err := users.ListUsers(ctx)
				if err != nil {
					return err
				}
				for _, u := range all {
					owners = append(owners, u.Name)
				}
			}
			for _, o := range owners {
				toks, err := tokens.List(ctx, o)
				if err != nil {
					return err
				}
				for _, t := range toks {
					expiry := "never expires"
					if t.Expires != nil {
						expiry = "expires " + t.Expires.Format("2006-01-02 15:04:05")
					}
					last := "never used"
					if t.LastUsedAt != nil {
						last = "last used " + t.LastUsedAt.Format("2006-01-02 15:04:05")
					}
					fmt.Printf("%s\t%s\t%s\t%s\t%s\n", o, t.Name, t.Prefix, expiry, last)
				}
			}
			return nil
		},
	}
	list.Flags().StringVar(&user, "user", "", "only list this user's tokens")

	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Revoke an API token.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, tokens, err := openTokenStores()
			if err != nil {
				return err
			}
			if user == "" {
				return errors.New("provide the owning user with --user")
			}
			if err := tokens.Remove(context.Background(), user, args[0]); err != nil {
				if errors.Is(err, auth.ErrNotFound) {
					return fmt.Errorf("token %q not found for user %q", args[0], user)
				}
				return err
			}
			fmt.Printf("removed token %q\n", args[0])
			return nil
		},
	}
	remove.Flags().StringVar(&user, "user", "", "the user that owns the token (required)")

	cmd.AddCommand(create, list, remove)
	return cmd
}

// openTokenStores opens the users and api_tokens tables from the configured
// server file. API tokens only exist when a database is configured.
func openTokenStores() (auth.UserStore, auth.TokenStore, error) {
	cfg, err := config.LoadServer(serverConfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid configuration: %w", err)
	}
	if cfg.Database.DSN == "" {
		return nil, nil, errors.New("no database.dsn in server.yml: API tokens are stored in the database")
	}
	d, err := db.Open(context.Background(), cfg.Database.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	return d.Users(), d.Tokens(), nil
}

// collectionCmd manages user-to-collection memberships. Once the first
// membership exists, only members can see and use their collections;
// until then every authenticated user has access (see internal/auth/access.go).
func collectionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collection",
		Short: "Manage user access to org/collection pairs.",
	}
	cmd.PersistentFlags().StringVarP(&serverConfigPath, "server-config", "s",
		"config/server.yml", "path to the server configuration file")

	grant := &cobra.Command{
		Use:   "grant <user> <org> <collection>",
		Short: "Allow a user to use one collection.",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openAccessStore()
			if err != nil {
				return err
			}
			if err := a.Add(context.Background(), args[0], args[1], args[2]); err != nil {
				return err
			}
			fmt.Printf("granted %s access to %s/%s\n", args[0], args[1], args[2])
			return nil
		},
	}

	revoke := &cobra.Command{
		Use:   "revoke <user> <org> <collection>",
		Short: "Remove a user's access to one collection.",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openAccessStore()
			if err != nil {
				return err
			}
			if err := a.Remove(context.Background(), args[0], args[1], args[2]); err != nil {
				return err
			}
			fmt.Printf("revoked %s access to %s/%s\n", args[0], args[1], args[2])
			return nil
		},
	}

	var user string
	list := &cobra.Command{
		Use:   "list",
		Short: "List collection memberships, for one user or all users.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := openAccessStore()
			if err != nil {
				return err
			}
			items, err := a.List(context.Background(), user)
			if err != nil {
				return err
			}
			for _, m := range items {
				fmt.Printf("%s\t%s/%s\n", m.Username, m.Org, m.Collection)
			}
			return nil
		},
	}
	list.Flags().StringVar(&user, "user", "", "only list this user's memberships")

	cmd.AddCommand(grant, revoke, list)
	return cmd
}

// openAccessStore opens the collection_members table from the configured
// server file. Memberships only exist when a database is configured.
func openAccessStore() (auth.CollectionAccess, error) {
	cfg, err := config.LoadServer(serverConfigPath)
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	if cfg.Database.DSN == "" {
		return nil, errors.New("no database.dsn in server.yml: collection memberships are stored in the database")
	}
	d, err := db.Open(context.Background(), cfg.Database.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	return d.Access(), nil
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
	workerOpts := []worker.ManagerOption{}
	if cfg.Server.Sandbox.Enabled {
		if !sandbox.KernelSupported() {
			logger.Warn("Landlock is not supported by this kernel: collection sandboxing will not be enforced")
		}
		manifests, err := sandbox.Build(cfg.Server.Sandbox, collMgr.Targets(), serverConfigPath, collectionConfigPath)
		if err != nil {
			return fmt.Errorf("build sandbox policies: %w", err)
		}
		workerOpts = append(workerOpts, worker.WithSandbox(manifests))
	}
	mgr, err := worker.NewManager(ctx, binPath, serverConfigPath, collectionConfigPath, cfg.Server.Workers, logger, workerOpts...)
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
	var users auth.UserStore
	var tokens auth.TokenStore
	var access auth.CollectionAccess
	if cfg.Server.Database.DSN != "" {
		dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		d, err := db.Open(dctx, cfg.Server.Database.DSN)
		cancel()
		if err != nil {
			_ = mgr.Stop()
			return fmt.Errorf("failed to open database: %w", err)
		}
		st = d
		users = d.Users()
		tokens = d.Tokens()
		access = d.Access()
		logger.Info("using PostgreSQL store")
	} else {
		st = store.NewMem()
		logger.Warn("no database DSN configured, using in-memory store")
	}

	apr := applier.NewAnsible(collMgr.Targets(), mgr, st, logger, notify.New(logger))

	// With a database, playbook runs flow through a postgres-backed River
	// queue and a River client in this process dispatches them to the worker
	// pool. Without a database, playbooks run directly. The scheduler below
	// decides when each individual (target, playbook) pair runs.
	var queue *riverqueue.Client
	var enqueue server.RunEnqueuer
	if cfg.Server.Database.DSN != "" {
		var err error
		queue, err = riverqueue.Open(ctx, cfg.Server.Database.DSN, cfg.Server.Workers, func(ctx context.Context, target, playbook string, runID uint64, check bool) error {
			return apr.Trigger(applier.WithCheck(applier.WithRunID(ctx, runID), check), target, playbook)
		}, logger)
		if err != nil {
			_ = mgr.Stop()
			return fmt.Errorf("failed to initialize job queue: %w", err)
		}
		enqueue = queue
	} else {
		enqueue = &directEnqueuer{apr: apr, logger: logger, cancels: map[string]context.CancelFunc{}}
	}

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
		record := store.RunRecord{Target: target, Playbook: playbook, State: store.RunStateQueued}
		if err := st.CreateRun(ctx, &record); err != nil {
			return err
		}
		jobID, err := enqueue.EnqueueRun(ctx, target, playbook, record.ID, false)
		if err != nil {
			return err
		}
		record.JobID = jobID
		if err := st.UpdateRun(ctx, record); err != nil {
			logger.Warn("failed to store job ID for scheduled run", zap.Uint64("run", record.ID), zap.Error(err))
		}
		return nil
	}
	sched := scheduler.New(cfg.Server.Interval, jobProvider, runScheduled, logger)
	var syncer server.Syncer
	if collMgr.HasGit() {
		syncer = collMgr
	}
	srv := server.New(cfg.Server.ListenAddr, apr, st, multi, enqueue, mgr, syncer, users, tokens, cfg.Server.Auth.Token, logger,
		server.WithCollectionAccess(access))

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
		zap.Duration("default_interval", cfg.Server.Interval),
		zap.Int("scheduled_jobs", len(jobProvider())),
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
// It still supports cancellation by tracking each run's context.
type directEnqueuer struct {
	apr     *applier.Ansible
	logger  *zap.Logger
	mu      sync.Mutex
	nextID  uint64
	cancels map[string]context.CancelFunc
}

func (d *directEnqueuer) EnqueueRun(_ context.Context, target, playbook string, runID uint64, check bool) (string, error) {
	d.mu.Lock()
	d.nextID++
	id := strconv.FormatUint(d.nextID, 10)
	ctx, cancel := context.WithTimeout(applier.WithCheck(applier.WithRunID(context.Background(), runID), check), 30*time.Minute)
	d.cancels[id] = cancel
	d.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			d.mu.Lock()
			delete(d.cancels, id)
			d.mu.Unlock()
		}()
		if err := d.apr.Trigger(ctx, target, playbook); err != nil && !errors.Is(err, context.Canceled) {
			d.logger.Error("run failed",
				zap.String("target", target),
				zap.String("playbook", playbook),
				zap.Error(err),
			)
		}
	}()
	return id, nil
}

// CancelRun cancels the context of a direct run.
func (d *directEnqueuer) CancelRun(_ context.Context, jobID string) error {
	d.mu.Lock()
	cancel, ok := d.cancels[jobID]
	d.mu.Unlock()
	if !ok {
		return errors.New("run is no longer active")
	}
	cancel()
	return nil
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

	cfg, err := config.Load(serverConfigPath, collectionConfigPath)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if manifest.Collection != "" {
		owned := make([]config.TargetConfig, 0, 1)
		for _, t := range cfg.Targets {
			if t.Name == manifest.Collection {
				owned = append(owned, t)
			}
		}
		if len(owned) != 1 {
			return fmt.Errorf("sandbox manifest names unknown collection %q", manifest.Collection)
		}
		cfg.Targets = owned
	}

	logger, err := newLogger(cfg.Server.LogLevel)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	if sandboxManifestPath != "" {
		if !sandbox.KernelSupported() {
			logger.Warn("Landlock is not supported by this kernel: sandboxing is not enforced")
		}
		if err := sandbox.Apply(manifest); err != nil {
			return fmt.Errorf("apply sandbox: %w", err)
		}
	}

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
