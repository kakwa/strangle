// Package collection manages the collection's targets. Git-backed targets
// are kept cloned into their configured path and pinned to a tracked ref
// (branch, tag or commit); a sync moves the checkout and reloads the
// target's strangle.yml, so per-branch content (templated paths such as
// `inventory: inventories/{{ .Branch }}.ini`) takes effect immediately.
package collection

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/kakwa/strangle/utils/cfg"
	"github.com/kakwa/strangle/utils/git"
	"go.uber.org/zap"
)

// Sentinel errors returned by Sync so callers (the API) can classify them.
var (
	// ErrUnknownTarget is returned for a sync of a name that is not in the
	// collection.
	ErrUnknownTarget = errors.New("unknown target")
	// ErrNotGit is returned for a sync of a target that has no git upstream.
	ErrNotGit = errors.New("target has no git upstream")
)

// Manager owns the resolved target configs and the git checkouts of the
// targets that have an upstream repository.
type Manager struct {
	mu      sync.RWMutex
	targets map[string]cfg.TargetConfig
	repos   map[string]*git.Repo
	refs    map[string]string // effective tracked ref per git target
	logger  *zap.Logger
}

// New builds a Manager from the resolved targets. Git-backed targets get a
// checkout pinned to their configured ref (branch shorthand honored).
func New(targets []cfg.TargetConfig, logger *zap.Logger) *Manager {
	m := &Manager{
		targets: map[string]cfg.TargetConfig{},
		repos:   map[string]*git.Repo{},
		refs:    map[string]string{},
		logger:  logger,
	}
	for _, t := range targets {
		m.targets[t.Name] = t
		if t.Git.IsSet() {
			ref := t.Git.EffectiveRef()
			m.refs[t.Name] = ref
			m.repos[t.Name] = git.New(t.Git.URL, ref, t.Path, t.Git.Token, logger)
		}
	}
	return m
}

// HasGit reports whether any target has an upstream repository.
func (m *Manager) HasGit() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.repos) > 0
}

// Targets returns the resolved target configs, sorted by name.
func (m *Manager) Targets() []cfg.TargetConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]cfg.TargetConfig, 0, len(m.targets))
	for _, t := range m.targets {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Target returns the resolved config of one target.
func (m *Manager) Target(name string) (cfg.TargetConfig, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.targets[name]
	return t, ok
}

// GitStatus reports the checkout state of a git-backed target.
type GitStatus struct {
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	Rev    string `json:"rev,omitempty"`
	Cloned bool   `json:"cloned"`
}

// GitStatus returns the checkout state of a git-backed target.
func (m *Manager) GitStatus(name string) (GitStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	repo, ok := m.repos[name]
	if !ok {
		return GitStatus{}, false
	}
	st := GitStatus{URL: repo.URL(), Ref: m.refs[name], Cloned: repo.IsCloned()}
	if st.Cloned {
		_, rev, err := repo.Status(context.Background())
		if err == nil {
			st.Rev = rev
		}
	}
	return st, true
}

// InitialSync syncs every git-backed target to its tracked ref. A failure on
// one target does not stop the others; the first error is returned so the
// caller can report it. Targets whose strangle.yml fails to load keep their
// previous (possibly empty) playbook list.
func (m *Manager) InitialSync(ctx context.Context) error {
	var firstErr error
	for _, name := range m.gitNames() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, _, err := m.Sync(ctx, name, ""); err != nil {
			m.logger.Error("initial collection sync failed",
				zap.String("target", name), zap.Error(err))
			if firstErr == nil {
				firstErr = fmt.Errorf("target %q: %w", name, err)
			}
		}
	}
	return firstErr
}

// gitNames returns the names of the git-backed targets, sorted.
func (m *Manager) gitNames() []string {
	names := make([]string, 0, len(m.repos))
	for name := range m.repos {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Sync moves the target's checkout to its tracked ref (or to branch when
// given, which becomes the new tracked ref) and reloads the target's
// strangle.yml with the current ref as the {{ .Branch }} template variable.
// It returns the new HEAD revision and whether the revision changed.
func (m *Manager) Sync(ctx context.Context, name, branch string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	repo, ok := m.repos[name]
	if !ok {
		if _, known := m.targets[name]; known {
			return "", false, fmt.Errorf("%w: %q", ErrNotGit, name)
		}
		return "", false, fmt.Errorf("%w: %q", ErrUnknownTarget, name)
	}
	if branch != "" {
		m.refs[name] = branch
		repo.SetRef(branch)
	}

	rev, changed, err := repo.Sync(ctx)
	if err != nil {
		return "", false, err
	}

	// Reload the target's strangle.yml from the fresh checkout. A broken
	// file does not roll the checkout back: the old playbook list is kept
	// and the error is reported, so a bad push is visible but cannot freeze
	// the target on an empty playbook list.
	t, err := m.loadTargetLocked(name, m.refs[name])
	if err != nil {
		m.logger.Error("reload strangle.yml after sync failed",
			zap.String("target", name), zap.Error(err))
		return rev, changed, err
	}
	m.targets[name] = t
	return rev, changed, nil
}

// Reload re-reads the strangle.yml of every git-backed target from its
// checkout without touching git. Worker processes, which share the master's
// checkouts, use it to resolve the playbooks their SSH managers are built
// from. A target whose file fails to load keeps its previous (possibly
// empty) playbook list; the first error is returned.
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for _, name := range m.gitNames() {
		t, err := m.loadTargetLocked(name, m.refs[name])
		if err != nil {
			m.logger.Error("reload strangle.yml failed",
				zap.String("target", name), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		m.targets[name] = t
	}
	return firstErr
}

// loadTargetLocked reads the target's strangle.yml from its checkout
// directory, applying branch templating. The caller must hold m.mu.
func (m *Manager) loadTargetLocked(name, ref string) (cfg.TargetConfig, error) {
	previous := m.targets[name]
	spec := cfg.TargetSpec{
		Name:        previous.Name,
		Org:         previous.Org,
		Path:        previous.Path,
		Git:         previous.Git,
		Retention:   previous.Retention,
		HostCleanup: previous.HostCleanup,
	}
	return cfg.LoadTarget(spec, previous.Path, map[string]string{
		"Name":   previous.Name,
		"Branch": ref,
	})
}
