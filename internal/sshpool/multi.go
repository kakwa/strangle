package sshpool

import (
	"fmt"
	"sort"
	"sync"
)

// Multi dispatches bridge requests to one of several per-target managers.
//
// A single embedded Python interpreter serves every target and all of their
// runs are serialized through one bridge, so a bridge request does not say
// which target it belongs to. Instead the runner marks the active target
// before every run (SetActive), and every request until the next switch is
// served by that target's manager.
type Multi struct {
	mu      sync.Mutex
	managers map[string]*Manager
	active   string
}

// NewMulti builds a dispatcher over the given per-target managers. A nil
// Multi is returned when no target uses the Go-managed SSH transport, in
// which case no connection plugin is installed.
func NewMulti(managers map[string]*Manager) (*Multi, error) {
	if len(managers) == 0 {
		return nil, nil
	}
	return &Multi{managers: managers}, nil
}

// SetActive marks the target whose manager will serve the bridge requests of
// the upcoming run. Callers must serialize runs (they do, through the
// applier's mutex).
func (m *Multi) SetActive(target string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.managers[target]; !ok {
		return fmt.Errorf("ssh: no SSH manager for target %q", target)
	}
	m.active = target
	return nil
}

// Active returns the currently active target name.
func (m *Multi) Active() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

func (m *Multi) activeManager() (*Manager, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == "" {
		return nil, fmt.Errorf("ssh: no active target (set one before running playbooks)")
	}
	return m.managers[m.active], nil
}

// Targets returns the names of all targets with an SSH manager.
func (m *Multi) Targets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.managers))
	for name := range m.managers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// manager returns the manager of one target, for API use.
func (m *Multi) manager(target string) (*Manager, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mgr, ok := m.managers[target]
	return mgr, ok
}

// Exec runs a command over a reused SSH connection of the active target.
func (m *Multi) Exec(ep Endpoint, command string, stdin []byte) (ExecResult, error) {
	mgr, err := m.activeManager()
	if err != nil {
		return ExecResult{}, err
	}
	return mgr.Exec(ep, command, stdin)
}

// Put writes a local file to the remote host over SFTP on the active target.
func (m *Multi) Put(ep Endpoint, remotePath string, content []byte, mode uint32) error {
	mgr, err := m.activeManager()
	if err != nil {
		return err
	}
	return mgr.Put(ep, remotePath, content, mode)
}

// Fetch reads a remote file over SFTP on the active target.
func (m *Multi) Fetch(ep Endpoint, remotePath string) ([]byte, error) {
	mgr, err := m.activeManager()
	if err != nil {
		return nil, err
	}
	return mgr.Fetch(ep, remotePath)
}

// PendingHostKeys aggregates the pending host keys of every target, tagged
// with their target name.
func (m *Multi) PendingHostKeys() []HostKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []HostKey
	for name, mgr := range m.managers {
		for _, key := range mgr.HostKeys().Pending() {
			key.Target = name
			out = append(out, key)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// ApproveHostKey approves a pending host key of one target.
func (m *Multi) ApproveHostKey(target, host, fingerprint string) error {
	mgr, ok := m.manager(target)
	if !ok {
		return fmt.Errorf("ssh: no SSH manager for target %q", target)
	}
	return mgr.HostKeys().Approve(host, fingerprint)
}

// RejectHostKey removes a pending host key of one target.
func (m *Multi) RejectHostKey(target, host string) error {
	mgr, ok := m.manager(target)
	if !ok {
		return fmt.Errorf("ssh: no SSH manager for target %q", target)
	}
	return mgr.HostKeys().Reject(host)
}

// Stats returns the pool counters of every target, keyed by target name.
func (m *Multi) Stats() map[string]Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Stats, len(m.managers))
	for name, mgr := range m.managers {
		out[name] = mgr.Stats()
	}
	return out
}

// Close closes the pooled connections of every target.
func (m *Multi) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mgr := range m.managers {
		mgr.Close()
	}
}
