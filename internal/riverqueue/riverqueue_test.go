package riverqueue

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"go.uber.org/zap"
)

func TestPlaybookRunArgsKind(t *testing.T) {
	if got := (PlaybookRunArgs{}).Kind(); got != jobKind {
		t.Errorf("Kind() = %q, want %q", got, jobKind)
	}
}

func TestPlaybookWorkerWork(t *testing.T) {
	var gotTarget, gotPlaybook string
	w := &PlaybookWorker{
		Run: func(_ context.Context, target, playbook string) error {
			gotTarget, gotPlaybook = target, playbook
			return nil
		},
		RunWithID: func(_ context.Context, target, playbook string, _ uint64) error {
			gotTarget, gotPlaybook = target, playbook
			return nil
		},
	}
	job := &river.Job[PlaybookRunArgs]{Args: PlaybookRunArgs{Target: "home", Playbook: "site.yml"}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if gotTarget != "home" || gotPlaybook != "site.yml" {
		t.Errorf("Work() called with (%q, %q), want (home, site.yml)", gotTarget, gotPlaybook)
	}

	// Errors from the run function propagate so River can retry.
	boom := errors.New("boom")
	w = &PlaybookWorker{Run: func(context.Context, string, string) error { return boom }}
	if err := w.Work(context.Background(), job); !errors.Is(err, boom) {
		t.Errorf("Work() error = %v, want %v", err, boom)
	}

	// A worker without a run function is a misconfiguration, not a retryable error.
	w = &PlaybookWorker{}
	if err := w.Work(context.Background(), job); err == nil {
		t.Error("Work() with nil Run: want error, got nil")
	}
}

// openTestClient opens the queue against a private, freshly created database
// so the test never contends with (or steals jobs from) other River clients
// on the shared development database.
func openTestClient(t *testing.T, dsn string, workers int, run func(ctx context.Context, target, playbook string, runID uint64) error) *Client {
	t.Helper()
	ctx := context.Background()

	baseURL, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	base, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect base: %v", err)
	}
	t.Cleanup(func() { base.Close() })

	dbName := fmt.Sprintf("strangle_river_test_%d", time.Now().UnixNano())
	if _, err := base.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = base.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
	})

	testURL := *baseURL
	testURL.Path = "/" + dbName

	c, err := Open(ctx, testURL.String(), workers, run, zap.NewNop())
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestClientEndToEnd is the full path: open (with migrations), enqueue,
// start, work and stop. It runs only when STRANGLE_TEST_DSN is set, like the
// other database-backed tests.
func TestClientEndToEnd(t *testing.T) {
	dsn := os.Getenv("STRANGLE_TEST_DSN")
	if dsn == "" {
		t.Skip("STRANGLE_TEST_DSN not set")
	}
	ctx := context.Background()

	calls := make(chan [2]string, 16)
	c := openTestClient(t, dsn, 2, func(_ context.Context, target, playbook string, _ uint64) error {
		calls <- [2]string{target, playbook}
		return nil
	})

	if jobID, err := c.EnqueueRun(ctx, "t1", "pb1", 11); err != nil {
		t.Fatalf("EnqueueRun() error: %v", err)
	} else if _, err := strconv.ParseInt(jobID, 10, 64); err != nil {
		t.Errorf("EnqueueRun() job ID = %q, want a numeric River ID: %v", jobID, err)
	}
	if _, err := c.EnqueueRun(ctx, "t2", "pb2", 12); err != nil {
		t.Fatalf("EnqueueRun() #2 error: %v", err)
	}

	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	want := map[[2]string]bool{{"t1", "pb1"}: true, {"t2", "pb2"}: true}
	seen := map[[2]string]bool{}
	timeout := time.After(30 * time.Second)
	for len(seen) < len(want) {
		select {
		case pair := <-calls:
			seen[pair] = true
		case <-timeout:
			t.Fatalf("timed out waiting for jobs, saw %v", seen)
		}
	}

	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

// TestClientFailedRunRetries checks that a failing run is retried by River
// (MaxAttempts > 1) before the job is finally discarded.
func TestClientFailedRunRetries(t *testing.T) {
	dsn := os.Getenv("STRANGLE_TEST_DSN")
	if dsn == "" {
		t.Skip("STRANGLE_TEST_DSN not set")
	}
	ctx := context.Background()

	attempts := make(chan struct{}, 16)
	c := openTestClient(t, dsn, 1, func(context.Context, string, string, uint64) error {
		attempts <- struct{}{}
		return errors.New("always fails")
	})

	if _, err := c.EnqueueRun(ctx, "t1", "pb1", 13); err != nil {
		t.Fatalf("EnqueueRun() error: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	// All maxAttempts attempts must have run before we stop.
	timeout := time.After(30 * time.Second)
	for n := 0; n < maxAttempts; n++ {
		select {
		case <-attempts:
		case <-timeout:
			t.Fatalf("timed out: %d of %d attempts seen", n, maxAttempts)
		}
	}

	if err := c.Stop(ctx); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
}

// TestOpenUnreachable covers Open's failure path: the pool is created lazily,
// so the migration is what surfaces the connection error.
func TestOpenUnreachable(t *testing.T) {
	_, err := Open(context.Background(), "postgres://nobody:nothing@127.0.0.1:1/strangle", 1,
		func(context.Context, string, string, uint64) error { return nil }, zap.NewNop())
	if err == nil {
		t.Fatal("Open() against an unreachable database: want error, got nil")
	}
}
