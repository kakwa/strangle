package riverqueue_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/riverqueue"
	"github.com/kakwa/strangle/internal/testutil"
	"github.com/riverqueue/river"
	"go.uber.org/zap"
)

// Mirrors of the queue's internal job metadata. They are kept here so the
// integration suite can stay in an external test package.
const (
	jobKind     = "playbook_run"
	maxAttempts = 3
)

func TestPlaybookRunArgsKind(t *testing.T) {
	if got := (riverqueue.PlaybookRunArgs{}).Kind(); got != jobKind {
		t.Errorf("Kind() = %q, want %q", got, jobKind)
	}
}

func TestPlaybookWorkerWork(t *testing.T) {
	var gotTarget, gotPlaybook string
	var gotRunID uint64
	var gotCheck bool
	w := &riverqueue.PlaybookWorker{
		Run: func(_ context.Context, target, playbook string) error {
			gotTarget, gotPlaybook = target, playbook
			return nil
		},
		RunWithID: func(_ context.Context, target, playbook string, runID uint64, check bool) error {
			gotTarget, gotPlaybook, gotRunID, gotCheck = target, playbook, runID, check
			return nil
		},
	}
	job := &river.Job[riverqueue.PlaybookRunArgs]{Args: riverqueue.PlaybookRunArgs{
		Target: "home", Playbook: "site.yml", RunID: 7, Check: true,
	}}
	if err := w.Work(context.Background(), job); err != nil {
		t.Fatalf("Work() error: %v", err)
	}
	if gotTarget != "home" || gotPlaybook != "site.yml" || gotRunID != 7 || !gotCheck {
		t.Errorf("Work() called with (%q, %q, %d, %t), want (home, site.yml, 7, true)",
			gotTarget, gotPlaybook, gotRunID, gotCheck)
	}

	// Errors from the run function propagate so River can retry.
	boom := errors.New("boom")
	w = &riverqueue.PlaybookWorker{Run: func(context.Context, string, string) error { return boom }}
	if err := w.Work(context.Background(), job); !errors.Is(err, boom) {
		t.Errorf("Work() error = %v, want %v", err, boom)
	}

	// A worker without a run function is a misconfiguration, not a retryable error.
	w = &riverqueue.PlaybookWorker{}
	if err := w.Work(context.Background(), job); err == nil {
		t.Error("Work() with nil Run: want error, got nil")
	}
}

// openTestClient opens the queue against a private, freshly created database
// so the test never contends with (or steals jobs from) another River client.
func openTestClient(t *testing.T, workers int, run func(ctx context.Context, target, playbook string, runID uint64, check bool) error) *riverqueue.Client {
	t.Helper()
	dsn := testutil.TestDSN(t, "river")
	c, err := riverqueue.Open(context.Background(), dsn, workers, run, zap.NewNop())
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// TestClientEndToEnd is the full path: open (with migrations), enqueue,
// start, work and stop.
func TestClientEndToEnd(t *testing.T) {
	ctx := context.Background()

	calls := make(chan [2]string, 16)
	c := openTestClient(t, 2, func(_ context.Context, target, playbook string, _ uint64, _ bool) error {
		calls <- [2]string{target, playbook}
		return nil
	})

	if jobID, err := c.EnqueueRun(ctx, "t1", "pb1", 11, false); err != nil {
		t.Fatalf("EnqueueRun() error: %v", err)
	} else if _, err := strconv.ParseInt(jobID, 10, 64); err != nil {
		t.Errorf("EnqueueRun() job ID = %q, want a numeric River ID: %v", jobID, err)
	}
	if _, err := c.EnqueueRun(ctx, "t2", "pb2", 12, false); err != nil {
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
// before the job is finally discarded.
func TestClientFailedRunRetries(t *testing.T) {
	ctx := context.Background()

	attempts := make(chan struct{}, 16)
	c := openTestClient(t, 1, func(context.Context, string, string, uint64, bool) error {
		attempts <- struct{}{}
		return errors.New("always fails")
	})

	if _, err := c.EnqueueRun(ctx, "t1", "pb1", 13, false); err != nil {
		t.Fatalf("EnqueueRun() error: %v", err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

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
	_, err := riverqueue.Open(context.Background(), "postgres://nobody:nothing@127.0.0.1:1/strangle", 1,
		func(context.Context, string, string, uint64, bool) error { return nil }, zap.NewNop())
	if err == nil {
		t.Fatal("Open() against an unreachable database: want error, got nil")
	}
}
