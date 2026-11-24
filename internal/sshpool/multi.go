package sshpool

import (
	"fmt"
	"sort"
	"sync"
)

// Multi dispatches bridge requests to one of several per-playbook managers.
//
// A single embedded Python interpreter serves every target and all of their
// runs are serialized through one bridge, so a bridge request does not say
// which playbook it belongs to. Instead the runner marks the active scope
// before every run (SetActive), and every request until the next switch is
// served by that playbook's manager.
type Multi struct {
	mu       sync.Mutex
	managers map[string]*Manager
	active   string
}

// NewMulti builds a dispatcher over the given per-playbook managers. A nil
// Multi is returned when no playbook uses the Go-managed SSH transport, in
// which case no connection plugin is installed.
func NewMulti(managers map[string]*Manager) (*Multi, error) {
	if len(managers) == 0 {
		return nil, nil
	}
	return &Multi{managers: managers}, nil
}

// SetActive marks the playbook scope whose manager will serve the bridge
// requests of the upcoming run. Callers must serialize runs (they do, through
// the applier's mutex).
func (m *Multi) SetActive(scope string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.managers[scope]; !ok {
		return fmt.Errorf("ssh: no SSH manager for scope %q", scope)
	}
	m.active = scope
	return nil
}

// ClearActive drops the active scope. It is called before jobs that do not
// use the Go-managed transport so a stale manager cannot serve their requests.
func (m *Multi) ClearActive() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = ""
}

// Active returns the currently active SSH scope.
func (m *Multi) Active() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

func (m *Multi) activeManager() (*Manager, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == "" {
		return nil, fmt.Errorf("ssh: no active scope (set one before running playbooks)")
	}
	return m.managers[m.active], nil
}

// Targets returns the keys of all playbook scopes with an SSH manager.
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

// manager returns the manager of one playbook scope, for API use.
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

// PendingHostKeys aggregates the pending host keys of every playbook scope,
// tagged with their scope key (<target>/<playbook name>).
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

// ApproveHostKey approves a pending host key of one playbook scope.
func (m *Multi) ApproveHostKey(scope, host, fingerprint string) error {
	mgr, ok := m.manager(scope)
	if !ok {
		return fmt.Errorf("ssh: no SSH manager for scope %q", scope)
	}
	return mgr.HostKeys().Approve(host, fingerprint)
}

// RejectHostKey removes a pending host key of one playbook scope.
func (m *Multi) RejectHostKey(scope, host string) error {
	mgr, ok := m.manager(scope)
	if !ok {
		return fmt.Errorf("ssh: no SSH manager for scope %q", scope)
	}
	return mgr.HostKeys().Reject(host)
}

// Stats returns the pool counters of every playbook scope, keyed by scope.
func (m *Multi) Stats() map[string]Stats {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Stats, len(m.managers))
	for name, mgr := range m.managers {
		out[name] = mgr.Stats()
	}
	return out
}

// Close closes the pooled connections of every playbook scope.
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
