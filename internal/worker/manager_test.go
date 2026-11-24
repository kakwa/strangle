package worker

import (
	"context"
	"errors"
	"net"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"

	runnerv1 "github.com/kakwa/strangle/proto/strangle/v1"
)

// TestMain lets the test binary double as the fake worker process: the
// manager spawns "binPath worker --socket ...", so the tests pass the test
// binary itself as binPath and intercept the "worker" argument here.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		os.Exit(runFakeWorker(os.Args[2:]))
	}
	os.Exit(m.Run())
}

// runFakeWorker serves the Runner gRPC service on the given socket. It runs
// until SIGTERM, or for FAKE_WORKER_LIFETIME when set (crash-loop tests).
// With FAKE_WORKER_HUNG the Runner service is not registered, so Ping fails
// with Unimplemented while the connection stays up (a hung worker).
func runFakeWorker(args []string) int {
	var socket string
	for i := 0; i < len(args); i++ {
		if args[i] == "--socket" && i+1 < len(args) {
			socket = args[i+1]
		}
	}
	if socket == "" {
		return 1
	}

	lis, err := net.Listen("unix", socket)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		return 1
	}
	defer os.Remove(socket)

	srv := grpc.NewServer()
	if os.Getenv("FAKE_WORKER_HUNG") == "" {
		runnerv1.RegisterRunnerServer(srv, fakeRunner{})
	}

	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
		var lifetime <-chan time.Time
		if v := os.Getenv("FAKE_WORKER_LIFETIME"); v != "" {
			d, err := time.ParseDuration(v)
			if err == nil {
				lifetime = time.After(d)
			}
		}
		select {
		case <-sig:
			srv.GracefulStop()
		case <-lifetime:
			srv.Stop()
		}
		close(done)
	}()

	if err := srv.Serve(lis); err != nil {
		return 1
	}
	<-done
	return 0
}

// fakeRunner answers the Runner service with canned results.
type fakeRunner struct {
	runnerv1.UnimplementedRunnerServer
}

func (fakeRunner) Ping(context.Context, *runnerv1.PingRequest) (*runnerv1.PingResponse, error) {
	return &runnerv1.PingResponse{Ok: true}, nil
}

func (fakeRunner) RunPlaybook(context.Context, *runnerv1.RunPlaybookRequest) (*runnerv1.RunPlaybookResponse, error) {
	return &runnerv1.RunPlaybookResponse{ExitCode: 0, Output: "fake output"}, nil
}

// newFakeManager starts a manager whose workers are fake (this test binary).
// env sets environment variables for the worker child processes.
func newFakeManager(t *testing.T, n int, env map[string]string) *Manager {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	m, err := NewManager(context.Background(), os.Args[0], "/dev/null", "/dev/null", n, zap.NewNop())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop() })
	return m
}

// withFastSupervision shortens the supervision intervals for the duration of
// the test.
func withFastSupervision(t *testing.T) {
	t.Helper()
	oldHealth := healthInterval
	oldBase := restartBackoffBase
	oldMax := maxConsecutiveFailures
	oldReset := failureReset
	healthInterval = 100 * time.Millisecond
	restartBackoffBase = 10 * time.Millisecond
	maxConsecutiveFailures = 3
	failureReset = time.Minute
	t.Cleanup(func() {
		healthInterval = oldHealth
		restartBackoffBase = oldBase
		maxConsecutiveFailures = oldMax
		failureReset = oldReset
	})
}

// waitFor polls f until it is true or the timeout elapses.
func waitFor(t *testing.T, what string, f func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func pidOf(t *testing.T, m *Manager, id int) int {
	t.Helper()
	for _, st := range m.WorkerStatuses(context.Background()) {
		if st.ID == id {
			return st.PID
		}
	}
	t.Fatalf("worker slot %d not found", id)
	return 0
}

func TestRestartDelay(t *testing.T) {
	oldBase, oldMax := restartBackoffBase, restartBackoffMax
	restartBackoffBase, restartBackoffMax = 100*time.Millisecond, 1*time.Second
	defer func() { restartBackoffBase, restartBackoffMax = oldBase, oldMax }()

	cases := []struct {
		fails int
		want  time.Duration
	}{
		{0, 0},
		{1, 100 * time.Millisecond},
		{2, 200 * time.Millisecond},
		{3, 400 * time.Millisecond},
		{4, 800 * time.Millisecond},
		{5, 1 * time.Second}, // capped
		{10, 1 * time.Second},
	}
	for _, tc := range cases {
		if got := restartDelay(tc.fails); got != tc.want {
			t.Errorf("restartDelay(%d) = %v, want %v", tc.fails, got, tc.want)
		}
	}
}

func TestNewManager(t *testing.T) {
	m := newFakeManager(t, 2, nil)
	ctx := context.Background()

	sts := m.WorkerStatuses(ctx)
	if len(sts) != 2 {
		t.Fatalf("workers = %d, want 2", len(sts))
	}
	for _, st := range sts {
		if st.PID == 0 {
			t.Errorf("worker %d: pid = 0, want a live process", st.ID)
		}
		if !st.Alive {
			t.Errorf("worker %d: alive = false, want true", st.ID)
		}
		if st.Socket == "" {
			t.Errorf("worker %d: empty socket path", st.ID)
		}
	}

	if _, err := os.Stat(m.dir); err != nil {
		t.Errorf("socket dir %q: %v", m.dir, err)
	}
}

func TestNewManagerSpawnFailure(t *testing.T) {
	_, err := NewManager(context.Background(), "/nonexistent/strangle", "/dev/null", "/dev/null", 1, zap.NewNop())
	if err == nil {
		t.Fatal("NewManager() error = nil, want an error")
	}
}

func TestManagerRunPlaybook(t *testing.T) {
	m := newFakeManager(t, 2, nil)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		code, out, err := m.RunPlaybook(ctx, "", ".", "-m", "debug")
		if err != nil {
			t.Fatalf("RunPlaybook() error: %v", err)
		}
		if code != 0 || out != "fake output" {
			t.Errorf("RunPlaybook() = (%d, %q), want (0, \"fake output\")", code, out)
		}
	}

	// No run is in flight anymore.
	for _, st := range m.WorkerStatuses(ctx) {
		if st.Jobs != 0 {
			t.Errorf("worker %d: jobs = %d, want 0", st.ID, st.Jobs)
		}
	}
}

func TestManagerRunPlaybookAfterStop(t *testing.T) {
	m := newFakeManager(t, 1, nil)
	_ = m.Stop()

	_, _, err := m.RunPlaybook(context.Background(), "", ".", "-m", "debug")
	if err == nil || err.Error() != "no workers available" {
		t.Errorf("RunPlaybook() error = %v, want \"no workers available\"", err)
	}
}

func TestManagerRestartsDeadWorker(t *testing.T) {
	withFastSupervision(t)
	m := newFakeManager(t, 1, nil)
	ctx := context.Background()

	orig := pidOf(t, m, 0)
	_ = syscall.Kill(orig, syscall.SIGKILL)

	waitFor(t, "worker to be restarted", func() bool {
		pid := pidOf(t, m, 0)
		return pid != 0 && pid != orig
	}, 15*time.Second)

	st := m.WorkerStatuses(ctx)[0]
	if st.Restarts != 1 {
		t.Errorf("restarts = %d, want 1", st.Restarts)
	}
	if !st.Alive {
		t.Errorf("restarted worker: alive = false, want true")
	}
}

func TestManagerKillsUnresponsiveWorker(t *testing.T) {
	withFastSupervision(t)
	m := newFakeManager(t, 1, map[string]string{"FAKE_WORKER_HUNG": "1"})
	ctx := context.Background()

	orig := pidOf(t, m, 0)
	st := m.WorkerStatuses(ctx)[0]
	if st.PID == 0 {
		t.Fatal("hung worker: pid = 0, want a live process")
	}
	if st.Alive {
		t.Fatal("hung worker: alive = true, want false (Ping must fail)")
	}

	// One failed ping is tolerated; the second kills the process.
	m.checkHealth()
	m.checkHealth()

	waitFor(t, "hung worker to be restarted", func() bool {
		pid := pidOf(t, m, 0)
		return pid != 0 && pid != orig
	}, 15*time.Second)

	// The replacement is also hung, so it is not alive either.
	if st := m.WorkerStatuses(ctx)[0]; st.Alive {
		t.Errorf("replacement worker: alive = true, want false")
	}
}

func TestManagerSupervisorKillsUnresponsiveWorker(t *testing.T) {
	withFastSupervision(t)
	m := newFakeManager(t, 1, map[string]string{"FAKE_WORKER_HUNG": "1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	orig := pidOf(t, m, 0)

	// The supervisor pings every 100ms and kills after two failures.
	waitFor(t, "supervisor to restart the hung worker", func() bool {
		pid := pidOf(t, m, 0)
		return pid != 0 && pid != orig
	}, 15*time.Second)
}

func TestManagerGivesUpOnCrashLoop(t *testing.T) {
	withFastSupervision(t)
	// The workers die 100ms after coming up, so the slot exhausts its
	// maxConsecutiveFailures (3) restarts quickly.
	m := newFakeManager(t, 1, map[string]string{"FAKE_WORKER_LIFETIME": "100ms"})

	select {
	case <-m.Down():
	case <-time.After(30 * time.Second):
		t.Fatal("Down() was not closed after the worker crash-looped")
	}

	sts := m.WorkerStatuses(context.Background())
	if len(sts) != 1 || sts[0].PID != 0 {
		t.Errorf("workers = %+v, want one dead slot (pid 0)", sts)
	}
	if sts[0].Restarts < 1 {
		t.Errorf("restarts = %d, want at least 1", sts[0].Restarts)
	}
}

func TestManagerStopIsIdempotent(t *testing.T) {
	m := newFakeManager(t, 1, nil)

	if err := m.Stop(); err != nil {
		t.Fatalf("first Stop() error: %v", err)
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("second Stop() error: %v", err)
	}
	if _, err := os.Stat(m.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket dir %q still exists after Stop (stat err = %v)", m.dir, err)
	}
}
