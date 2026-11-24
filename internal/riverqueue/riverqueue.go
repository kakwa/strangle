// Package riverqueue runs playbook jobs through a postgres-backed River job
// queue. The master enqueues one job per (target, playbook) pair - from the
// scheduler cycle and from manual API triggers - and a River client in the
// master process consumes them, dispatching each run to the worker pool with
// at most as many concurrent runs as there are worker processes.
package riverqueue

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"go.uber.org/zap"
)

const (
	// QueueName is the name of the queue playbook runs are worked from.
	QueueName = "playbooks"

	// jobKind identifies playbook run jobs in the river_job table.
	jobKind = "playbook_run"

	// jobTimeout bounds one playbook run; a run that outlives it is canceled
	// and its result is still recorded by the applier.
	jobTimeout = 30 * time.Minute

	// maxAttempts is how many times a failed run is retried before River
	// discards the job. The next scheduler cycle enqueues the playbook again
	// regardless, so this only bounds immediate re-runs.
	maxAttempts = 3
)

// PlaybookRunArgs is the payload of one playbook run job.
type PlaybookRunArgs struct {
	// Target is the collection target name.
	Target string `json:"target"`
	// Playbook is the job name within the target (strangle.yml name).
	Playbook string `json:"playbook"`
}

// Kind implements river.JobArgs.
func (PlaybookRunArgs) Kind() string { return jobKind }

// PlaybookWorker works playbook run jobs.
type PlaybookWorker struct {
	river.WorkerDefaults[PlaybookRunArgs]
	// Run executes the named playbook of the named target and records its
	// result. In production it is the applier's Trigger.
	Run func(ctx context.Context, target, playbook string) error
}

// Work implements river.Worker.
func (w *PlaybookWorker) Work(ctx context.Context, job *river.Job[PlaybookRunArgs]) error {
	if w.Run == nil {
		return fmt.Errorf("riverqueue: worker has no run function")
	}
	return w.Run(ctx, job.Args.Target, job.Args.Playbook)
}

// Client is a handle on the postgres-backed River queue for playbook runs.
type Client struct {
	client *river.Client[pgx.Tx]
	pool   *pgxpool.Pool
}

// Open connects to the Postgres database at dsn, applies River's migrations
// and builds a client that works the playbook queue with at most workers
// concurrent runs. run executes one job when the queue hands it out.
func Open(ctx context.Context, dsn string, workers int, run func(ctx context.Context, target, playbook string) error, logger *zap.Logger) (*Client, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("riverqueue: connect: %w", err)
	}

	driver := riverpgxv5.New(pool)

	slogLogger := slog.New(newZapSlogHandler(logger.Core()))
	migrator, err := rivermigrate.New(driver, &rivermigrate.Config{Logger: slogLogger})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("riverqueue: new migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		pool.Close()
		return nil, fmt.Errorf("riverqueue: migrate: %w", err)
	}

	riverWorkers := river.NewWorkers()
	river.AddWorker(riverWorkers, &PlaybookWorker{Run: run})

	client, err := river.NewClient(driver, &river.Config{
		JobTimeout:  jobTimeout,
		MaxAttempts: maxAttempts,
		Queues: map[string]river.QueueConfig{
			QueueName: {MaxWorkers: workers},
		},
		Workers: riverWorkers,
		Logger:  slogLogger,
	})
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("riverqueue: new client: %w", err)
	}
	return &Client{client: client, pool: pool}, nil
}

// EnqueueRun adds a job that runs the named playbook of the named target.
func (c *Client) EnqueueRun(ctx context.Context, target, playbook string) (string, error) {
	_, err := c.client.Insert(ctx, PlaybookRunArgs{Target: target, Playbook: playbook}, &river.InsertOpts{
		Queue: QueueName,
	})
	return "queued", err
}

// Start begins fetching and working queued playbook runs. The context is
// only used for the initial startup; canceling it later does not cancel
// in-flight jobs (use Stop for a graceful drain, which is what a shutdown
// wants: running playbooks should finish and be recorded).
func (c *Client) Start(ctx context.Context) error {
	return c.client.Start(ctx)
}

// Stop gracefully shuts the client down: it stops fetching new jobs and
// waits for in-flight runs to complete, bounded by ctx.
func (c *Client) Stop(ctx context.Context) error {
	return c.client.Stop(ctx)
}

// Close releases the database pool.
func (c *Client) Close() {
	c.pool.Close()
}
