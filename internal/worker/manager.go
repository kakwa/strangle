package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"go.uber.org/zap"
)

// WorkerStatus describes one worker child process for API reporting.
type WorkerStatus struct {
	ID     int    `json:"id"`
	PID    int    `json:"pid"`
	Socket string `json:"socket"`
	Alive  bool   `json:"alive"`
	Jobs   int64  `json:"jobs"`
}

// Manager owns the worker child processes of the master: it spawns them,
// dials their unix sockets and dispatches playbook runs across them.
type Manager struct {
	binPath      string
	serverConfig string
	collConfig   string
	logger       *zap.Logger

	mu       sync.Mutex
	clients  []*Client
	procs    []*exec.Cmd
	jobs     map[*Client]int64
	rr       int
	stopping bool

	dir  string
	wg   sync.WaitGroup // watch goroutines
	down chan struct{}
	downOnce sync.Once
}

// NewManager spawns count worker child processes (binPath worker ...) and
// dials each of their unix sockets. The socket directory is created in the
// system temp dir and removed by Stop.
func NewManager(ctx context.Context, binPath, serverConfig, collConfig string, count int, logger *zap.Logger) (*Manager, error) {
	dir, err := os.MkdirTemp("", "strangle-workers-")
	if err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	m := &Manager{
		binPath:      binPath,
		serverConfig: serverConfig,
		collConfig:   collConfig,
		logger:       logger,
		dir:          dir,
		jobs:         map[*Client]int64{},
		down:         make(chan struct{}),
	}

	for i := 0; i < count; i++ {
		socket := filepath.Join(dir, fmt.Sprintf("worker-%d.sock", i))
		cmd := exec.Command(binPath, "worker",
			"--socket", socket,
			"-s", serverConfig,
			"-c", collConfig,
		)
		// Worker logs go to the master's stderr, tagged by process.
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // kill the whole group on stop
		if err := cmd.Start(); err != nil {
			_ = m.Stop()
			return nil, fmt.Errorf("spawn worker %d: %w", i, err)
		}

		cli, err := Dial(ctx, socket)
		if err != nil {
			_ = m.Stop()
			return nil, fmt.Errorf("dial worker %d: %w", i, err)
		}

		m.clients = append(m.clients, cli)
		m.procs = append(m.procs, cmd)
		m.wg.Add(1)
		go m.watch(i, cmd)
		logger.Info("worker started", zap.Int("id", i), zap.Int("pid", cmd.Process.Pid), zap.String("socket", socket))
	}

	return m, nil
}

// watch waits for one worker process and reports an unexpected exit. Stop is
// responsible for killing the processes; watch only reaps them.
func (m *Manager) watch(i int, cmd *exec.Cmd) {
	defer m.wg.Done()
	if err := cmd.Wait(); err != nil {
		m.logger.Warn("worker exited", zap.Int("id", i), zap.Error(err))
	}
	m.mu.Lock()
	stopping := m.stopping
	m.mu.Unlock()
	if !stopping {
		m.downOnce.Do(func() {
			m.logger.Error("worker died unexpectedly", zap.Int("id", i))
			close(m.down)
		})
	}
}

// RunPlaybook dispatches one playbook to the next worker (round-robin) and
// returns its exit code and captured output. Runs on one worker are
// serialized inside that worker.
func (m *Manager) RunPlaybook(ctx context.Context, scope, workingDir string, args ...string) (int, string, error) {
	m.mu.Lock()
	if m.stopping || len(m.clients) == 0 {
		m.mu.Unlock()
		return 0, "", fmt.Errorf("no workers available")
	}
	cli := m.clients[m.rr%len(m.clients)]
	m.rr++
	m.jobs[cli]++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.jobs[cli]--
		m.mu.Unlock()
	}()

	code, output, err := cli.RunPlaybook(ctx, scope, workingDir, args...)
	if err != nil {
		return 0, output, err
	}
	return int(code), output, nil
}

// Status returns one entry per worker, with liveness probed over gRPC.
func (m *Manager) Status(ctx context.Context) []WorkerStatus {
	m.mu.Lock()
	clients := append([]*Client(nil), m.clients...)
	procs := append([]*exec.Cmd(nil), m.procs...)
	jobs := make(map[*Client]int64, len(m.jobs))
	for c, n := range m.jobs {
		jobs[c] = n
	}
	m.mu.Unlock()

	out := make([]WorkerStatus, 0, len(clients))
	for i, cli := range clients {
		alive := true
		if err := cli.Ping(ctx); err != nil {
			alive = false
		}
		out = append(out, WorkerStatus{
			ID:     i,
			PID:    procs[i].Process.Pid,
			Socket: socketOf(procs[i]),
			Alive:  alive,
			Jobs:   jobs[cli],
		})
	}
	return out
}

// Down returns a channel closed when a worker process exits unexpectedly.
// The master uses it to restart the worker.
func (m *Manager) Down() <-chan struct{} {
	return m.down
}

// Stop terminates every worker child process, closes the gRPC connections
// and removes the socket directory. It is safe to call more than once.
func (m *Manager) Stop() error {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil
	}
	m.stopping = true
	clients := m.clients
	procs := m.procs
	m.mu.Unlock()

	for _, cli := range clients {
		_ = cli.Close()
	}
	for _, cmd := range procs {
		// Kill the whole process group: the worker spawns no children of
		// its own today, but this keeps it so if it ever does.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	m.wg.Wait()
	_ = os.RemoveAll(m.dir)
	return nil
}

// socketOf recovers the socket path passed to a worker process.
func socketOf(cmd *exec.Cmd) string {
	for i, arg := range cmd.Args {
		if i > 0 && cmd.Args[i-1] == "--socket" {
			return arg
		}
	}
	return ""
}
