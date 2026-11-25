// Package worker runs strangle playbook jobs: the worker process executes
// playbooks in its embedded interpreter (worker.go), while the master side
// spawns, supervises and dispatches to worker child processes (manager.go).
// Master/worker communication lives in internal/workeripc (JSON-RPC).
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/kakwa/strangle/internal/sandbox"
	"github.com/kakwa/strangle/internal/workeripc"
	"go.uber.org/zap"
)

// WorkerStatus describes one worker slot for API reporting. A slot is the
// fixed place in the pool of a worker process: the process (and its PID) may
// be replaced by a restart, but the slot keeps its id and job counters.
type WorkerStatus struct {
	ID         int    `json:"id"`
	PID        int    `json:"pid"`
	Socket     string `json:"socket"`
	Collection string `json:"collection,omitempty"`
	Alive      bool   `json:"alive"`
	Jobs       int64  `json:"jobs"`
	Restarts   int    `json:"restarts"`
}

// Supervision tuning. Package variables so tests can shorten the intervals.
var (
	healthInterval         = 10 * time.Second
	pingTimeout            = 10 * time.Second
	pingFailuresToKill     = 2 // consecutive failed pings before a live process is killed
	restartBackoffBase     = 500 * time.Millisecond
	restartBackoffMax      = 30 * time.Second
	maxConsecutiveFailures = 10
	failureReset           = 5 * time.Minute // a worker up this long is forgiven its failures
)

// restartDelay is the backoff before restart attempt number n (n is the
// number of consecutive failures). The first attempt is immediate.
func restartDelay(fails int) time.Duration {
	if fails <= 0 {
		return 0
	}
	d := restartBackoffBase
	for i := 1; i < fails; i++ {
		d *= 2
		if d >= restartBackoffMax {
			return restartBackoffMax
		}
	}
	return d
}

// slot is one fixed place in the worker pool: its socket path, the current
// process and connection, and its supervision state.
type slot struct {
	id         int
	socket     string
	collection string    // non-empty for sandboxed workers pinned to a collection
	manifest   string    // sandbox manifest path, empty when sandboxing is off
	cmd        *exec.Cmd // current process, nil while (re)spawning or dead
	cli        *workeripc.Client
	fails      int // consecutive failures since the last clean run
	restarts   int
	lastStart  time.Time // when the current process came up
	pingFails  int
	dead       bool // gave up after maxConsecutiveFailures consecutive failures
}

// ManagerOption customises a worker pool at construction time.
type ManagerOption func(*Manager)

// WithSandbox pins workers to collections and applies a Landlock manifest to
// each worker process. The map is keyed by collection name; when set, the
// manager spawns count workers per collection instead of one shared pool.
func WithSandbox(manifests map[string]sandbox.Manifest) ManagerOption {
	return func(m *Manager) { m.sandbox = manifests }
}

// Manager owns the worker child processes of the master: it spawns them,
// dials their unix sockets, dispatches playbook runs across them and keeps
// them alive. A worker that exits unexpectedly or stops answering health
// pings is restarted, with a growing backoff; a slot that keeps failing is
// given up for, and Down() is closed.
type Manager struct {
	binPath      string
	serverConfig string
	collConfig   string
	ctx          context.Context // used to spawn workers (dialing)
	logger       *zap.Logger

	mu       sync.Mutex
	slots    []*slot
	jobs     map[int]int64 // in-flight runs per slot id
	rr       int
	stopping bool
	down     chan struct{}
	downOnce sync.Once
	stopCh   chan struct{}

	sandbox map[string]sandbox.Manifest // nil when sandboxing is disabled

	dir string
	wg  sync.WaitGroup // watch and supervise goroutines
}

// NewManager spawns count worker child processes (binPath worker ...) and
// dials each of their unix sockets. The socket directory is created in the
// system temp dir and removed by Stop.
func NewManager(ctx context.Context, binPath, serverConfig, collConfig string, count int, logger *zap.Logger, opts ...ManagerOption) (*Manager, error) {
	dir, err := os.MkdirTemp("", "strangle-workers-")
	if err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	m := &Manager{
		binPath:      binPath,
		serverConfig: serverConfig,
		collConfig:   collConfig,
		ctx:          ctx,
		logger:       logger,
		dir:          dir,
		jobs:         map[int]int64{},
		down:         make(chan struct{}),
		stopCh:       make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}

	// Without sandboxing there is one shared pool. With sandboxing each
	// collection gets count dedicated workers, each with its own manifest
	// and scratch directory, so a playbook cannot see a sibling collection's
	// files or credentials.
	if m.sandbox == nil {
		for i := 0; i < count; i++ {
			if err := m.addSlot(&slot{id: i, socket: filepath.Join(dir, fmt.Sprintf("worker-%d.sock", i))}); err != nil {
				_ = m.Stop()
				return nil, err
			}
		}
	} else {
		names := make([]string, 0, len(m.sandbox))
		for name := range m.sandbox {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for i := 0; i < count; i++ {
				scratch := filepath.Join(dir, "scratch-"+safeName(name)+"-"+strconv.Itoa(i))
				s := &slot{
					id:         len(m.slots),
					collection: name,
					socket:     filepath.Join(scratch, "worker.sock"),
				}
				manifest, err := m.slotManifest(name, scratch)
				if err != nil {
					_ = m.Stop()
					return nil, err
				}
				path := filepath.Join(dir, fmt.Sprintf("sandbox-%s-%d.json", safeName(name), i))
				if err := manifest.WriteFile(path); err != nil {
					_ = m.Stop()
					return nil, fmt.Errorf("write sandbox manifest: %w", err)
				}
				s.manifest = path
				if err := m.addSlot(s); err != nil {
					_ = m.Stop()
					return nil, err
				}
			}
		}
	}

	return m, nil
}

// addSlot starts one worker and its supervision goroutine.
func (m *Manager) addSlot(s *slot) error {
	m.slots = append(m.slots, s)
	if err := m.start(s); err != nil {
		return fmt.Errorf("spawn worker %d: %w", s.id, err)
	}
	m.mu.Lock()
	pid := s.cmd.Process.Pid
	m.mu.Unlock()
	m.wg.Add(1)
	go m.watch(s)
	m.logger.Info("worker started",
		zap.Int("id", s.id), zap.Int("pid", pid), zap.String("socket", s.socket),
		zap.String("collection", s.collection))
	return nil
}

// slotManifest clones the collection's base policy, adding this worker's
// private scratch directory. The unix socket lives inside it, so granting
// access to it does not expose another worker's scratch or socket.
func (m *Manager) slotManifest(name, scratch string) (sandbox.Manifest, error) {
	base := m.sandbox[name]
	for _, dir := range []string{scratch, filepath.Join(scratch, "tmp"), filepath.Join(scratch, "home"), filepath.Join(scratch, "cache")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return base, fmt.Errorf("create sandbox scratch dir %q: %w", dir, err)
		}
	}
	base.Rules = append(base.Rules,
		sandbox.Rule{Path: scratch, Mode: sandbox.ModeRW, Kind: sandbox.KindDir, Refer: true, ResolveUnix: true},
	)
	env := map[string]string{
		"TMPDIR":              filepath.Join(scratch, "tmp"),
		"HOME":                filepath.Join(scratch, "home"),
		"ANSIBLE_LOCAL_TEMP":  filepath.Join(scratch, "tmp"),
		"ANSIBLE_HOME":        filepath.Join(scratch, "cache"),
		"PYTHONPYCACHEPREFIX": filepath.Join(scratch, "cache"),
	}
	for k, v := range base.Env {
		env[k] = v
	}
	base.Env = env
	return base, nil
}

func safeName(name string) string {
	out := []rune(name)
	for i, r := range out {
		if r != '.' && r != '_' && r != '-' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			out[i] = '_'
		}
	}
	return string(out)
}

// start spawns one worker process, dials its socket and publishes the pair
// on the slot. It is called from NewManager and from the slot's watch
// goroutine.
func (m *Manager) start(s *slot) error {
	// A killed worker leaves its socket file behind; remove it before the
	// replacement listens on the same path.
	_ = os.Remove(s.socket)

	args := []string{"worker", "--socket", s.socket, "-s", m.serverConfig, "-c", m.collConfig}
	if s.manifest != "" {
		args = append(args, "--sandbox", s.manifest)
	}
	cmd := exec.Command(m.binPath, args...)
	// Worker logs go to the master's stderr, tagged by process.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // kill the whole group on stop
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}

	cli, err := workeripc.Dial(m.ctx, s.socket)
	if err != nil {
		// Kill the whole group: the process is ours (Setpgid) and may
		// have started children of its own.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("dial: %w", err)
	}

	m.mu.Lock()
	stopping := m.stopping
	if !stopping {
		s.cmd = cmd
		s.cli = cli
		s.lastStart = time.Now()
	}
	m.mu.Unlock()

	if stopping {
		_ = cli.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}
	return nil
}

// watch reaps the worker process of one slot and restarts it on unexpected
// exit. It runs for the life of the slot.
func (m *Manager) watch(s *slot) {
	defer m.wg.Done()

	for {
		m.mu.Lock()
		cmd := s.cmd
		m.mu.Unlock()
		if cmd == nil {
			return // stopping or permanently dead
		}
		if err := cmd.Wait(); err != nil {
			m.logger.Warn("worker exited",
				zap.Int("id", s.id), zap.Int("pid", cmd.Process.Pid), zap.Error(err))
		}
		if !m.replace(s) {
			return
		}
	}
}

// replace retires the just-exited process of the slot and spawns a
// replacement, backing off with each consecutive failure. It returns false
// when the slot will not be restarted (stopping, or the slot gave up after
// maxConsecutiveFailures consecutive failures).
func (m *Manager) replace(s *slot) bool {
	m.mu.Lock()
	if s.cli != nil {
		_ = s.cli.Close()
		s.cli = nil
	}
	s.cmd = nil
	s.fails++
	if s.fails > maxConsecutiveFailures {
		s.dead = true
		m.mu.Unlock()
		m.giveUp(s)
		return false
	}
	s.restarts++
	m.mu.Unlock()

	if !m.sleepCtx(restartDelay(s.fails)) {
		return false
	}

	for {
		m.mu.Lock()
		stopping := m.stopping
		m.mu.Unlock()
		if stopping {
			return false
		}

		if err := m.start(s); err != nil {
			m.mu.Lock()
			stopping := m.stopping
			if !stopping {
				s.fails++
			}
			gaveUp := s.fails > maxConsecutiveFailures
			if gaveUp && !stopping {
				s.dead = true
			}
			m.mu.Unlock()
			if stopping {
				return false
			}
			if gaveUp {
				m.giveUp(s)
				return false
			}
			m.logger.Error("worker restart failed", zap.Int("id", s.id), zap.Error(err))
			if !m.sleepCtx(restartDelay(s.fails)) {
				return false
			}
			continue
		}

		m.mu.Lock()
		if s.cmd == nil {
			// start bailed out while the manager was stopping.
			m.mu.Unlock()
			return false
		}
		pid := s.cmd.Process.Pid
		m.mu.Unlock()
		m.logger.Info("worker restarted", zap.Int("id", s.id), zap.Int("pid", pid))
		return true
	}
}

// giveUp reports that the slot is permanently down.
func (m *Manager) giveUp(s *slot) {
	m.mu.Lock()
	fails := s.fails
	m.mu.Unlock()
	m.logger.Error("worker permanently down after consecutive failures",
		zap.Int("id", s.id), zap.Int("failures", fails))
	m.downOnce.Do(func() { close(m.down) })
}

// sleepCtx sleeps for d, returning false if the manager is stopping first.
func (m *Manager) sleepCtx(d time.Duration) bool {
	if d <= 0 {
		m.mu.Lock()
		stopping := m.stopping
		m.mu.Unlock()
		return !stopping
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-m.stopCh:
		return false
	case <-t.C:
		return true
	}
}

// Start begins the supervisor: it periodically pings every worker and
// restarts a worker that stops answering, so a hung process (alive but not
// serving) is replaced just like a dead one. It returns when ctx is
// canceled or the manager stops.
func (m *Manager) Start(ctx context.Context) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.supervise(ctx)
	}()
}

func (m *Manager) supervise(ctx context.Context) {
	t := time.NewTicker(healthInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-t.C:
		}
		m.checkHealth()
	}
}

// checkHealth pings every live worker. A worker that does not answer
// pingFailuresToKill times in a row is killed; its watch goroutine then
// restarts it. A ping also forgives the slot's failures once the worker has
// stayed up longer than failureReset.
func (m *Manager) checkHealth() {
	m.mu.Lock()
	slots := make([]*slot, len(m.slots))
	copy(slots, m.slots)
	m.mu.Unlock()

	for _, s := range slots {
		m.mu.Lock()
		cli := s.cli
		cmd := s.cmd
		m.mu.Unlock()
		if cli == nil || cmd == nil {
			continue // dead, stopping, or (re)spawning
		}

		pctx, cancel := context.WithTimeout(m.ctx, pingTimeout)
		err := cli.Ping(pctx)
		cancel()
		if err != nil {
			m.mu.Lock()
			s.pingFails++
			kill := s.pingFails >= pingFailuresToKill
			if kill {
				s.pingFails = 0
			}
			m.mu.Unlock()
			if kill {
				m.logger.Error("worker unresponsive, killing it for restart",
					zap.Int("id", s.id), zap.Error(err))
				// SIGKILL: a hung worker may not answer a graceful stop.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			} else {
				m.logger.Warn("worker did not answer ping", zap.Int("id", s.id), zap.Error(err))
			}
			continue
		}

		m.mu.Lock()
		s.pingFails = 0
		if s.fails > 0 && time.Since(s.lastStart) > failureReset {
			s.fails = 0
		}
		m.mu.Unlock()
	}
}

// RunPlaybook dispatches one playbook to the next live worker (round-robin,
// skipping dead or respawning slots) and returns its exit code, captured
// output and per-host facts. With sandboxing enabled only workers dedicated
// to collection are considered. Runs on one worker are serialized inside
// that worker.
func (m *Manager) RunPlaybook(ctx context.Context, collection, scope, workingDir string, args ...string) (int, string, map[string]json.RawMessage, error) {
	m.mu.Lock()
	if m.stopping || len(m.slots) == 0 {
		m.mu.Unlock()
		return 0, "", nil, fmt.Errorf("no workers available")
	}
	n := len(m.slots)
	var s *slot
	var cli *workeripc.Client
	for i := 0; i < n; i++ {
		c := m.slots[(m.rr+i)%n]
		if c.dead || c.cli == nil {
			continue
		}
		if m.sandbox != nil && c.collection != collection {
			continue
		}
		s = c
		cli = c.cli
		m.rr = (m.rr + i + 1) % n
		m.jobs[c.id]++
		break
	}
	m.mu.Unlock()
	if cli == nil {
		if m.sandbox != nil {
			return 0, "", nil, fmt.Errorf("no worker available for collection %q", collection)
		}
		return 0, "", nil, fmt.Errorf("no workers available")
	}
	defer func() {
		m.mu.Lock()
		m.jobs[s.id]--
		m.mu.Unlock()
	}()

	code, output, facts, err := cli.RunPlaybook(ctx, collection, scope, workingDir, args...)
	if err != nil {
		return 0, output, facts, err
	}
	return code, output, facts, nil
}

// WorkerStatuses returns one entry per worker slot, with liveness probed
// over the worker's JSON-RPC API.
func (m *Manager) WorkerStatuses(ctx context.Context) []WorkerStatus {
	m.mu.Lock()
	slots := make([]*slot, len(m.slots))
	copy(slots, m.slots)
	jobs := make(map[int]int64, len(m.jobs))
	for id, n := range m.jobs {
		jobs[id] = n
	}
	m.mu.Unlock()

	out := make([]WorkerStatus, 0, len(slots))
	for _, s := range slots {
		m.mu.Lock()
		cli := s.cli
		pid := 0
		if s.cmd != nil {
			pid = s.cmd.Process.Pid
		}
		restarts := s.restarts
		m.mu.Unlock()

		alive := cli != nil && cli.Ping(ctx) == nil
		out = append(out, WorkerStatus{
			ID:         s.id,
			PID:        pid,
			Socket:     s.socket,
			Collection: s.collection,
			Alive:      alive,
			Jobs:       jobs[s.id],
			Restarts:   restarts,
		})
	}
	return out
}

// Down returns a channel closed when a worker slot is permanently down (it
// exhausted maxConsecutiveFailures consecutive restart attempts).
func (m *Manager) Down() <-chan struct{} {
	return m.down
}

// Stop terminates every worker child process, closes the client connections
// and removes the socket directory. It is safe to call more than once.
func (m *Manager) Stop() error {
	m.mu.Lock()
	if m.stopping {
		m.mu.Unlock()
		return nil
	}
	m.stopping = true
	m.mu.Unlock()
	close(m.stopCh)

	m.mu.Lock()
	var clients []*workeripc.Client
	var procs []*exec.Cmd
	for _, s := range m.slots {
		if s.cli != nil {
			clients = append(clients, s.cli)
			s.cli = nil
		}
		if s.cmd != nil {
			procs = append(procs, s.cmd)
			s.cmd = nil
		}
	}
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
