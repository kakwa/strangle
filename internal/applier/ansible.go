// Package applier runs Ansible playbooks on a schedule and records their
// results: run metadata, per-host recap lines and per-task change traces.
package applier

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/inventory"
	"github.com/kakwa/strangle/internal/parser"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// Executor runs one playbook and reports its outcome: the ansible-playbook
// exit code, the captured output and an error for failures outside the
// interpreter (setup, transport or a closed worker). The master's worker
// pool implements it; tests can provide their own.
type Executor interface {
	RunPlaybook(ctx context.Context, scope, workingDir string, args ...string) (int, string, error)
}

// Ansible applies playbooks for every configured target by dispatching each
// run to a worker process and records the results in a store.
type Ansible struct {
	mu      sync.RWMutex
	targets map[string]config.TargetConfig
	exec    Executor
	store   store.Store
	logger  *zap.Logger
}

// NewAnsible returns an Applier for the given targets. Runs are executed by
// the given executor (a pool of worker processes in production).
func NewAnsible(targets []config.TargetConfig, exec Executor, st store.Store, logger *zap.Logger) *Ansible {
	a := &Ansible{
		targets: map[string]config.TargetConfig{},
		exec:    exec,
		store:   st,
		logger:  logger,
	}
	for _, t := range targets {
		a.targets[t.Name] = t
	}
	return a
}

// UpdateTargets updates the configured targets, e.g. after a git sync has
// reloaded a target's strangle.yml.
func (a *Ansible) UpdateTargets(targets []config.TargetConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range targets {
		a.targets[t.Name] = t
	}
}

// TargetNames returns the names of all configured targets, sorted.
func (a *Ansible) TargetNames() []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	names := make([]string, 0, len(a.targets))
	for name := range a.targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Playbooks returns the names of the configured jobs of one target.
func (a *Ansible) Playbooks(target string) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	t, ok := a.targets[target]
	if !ok {
		return nil
	}
	names := make([]string, 0, len(t.Playbooks))
	for _, pb := range t.Playbooks {
		names = append(names, pb.Name)
	}
	return names
}

// PlaybookConfigs returns the configured jobs of one target, in order.
func (a *Ansible) PlaybookConfigs(target string) []config.PlaybookConfig {
	a.mu.RLock()
	defer a.mu.RUnlock()
	t, ok := a.targets[target]
	if !ok {
		return nil
	}
	return t.Playbooks
}

// KnownPlaybook reports whether name is one of the target's job names.
func (a *Ansible) KnownPlaybook(target, name string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	t, ok := a.targets[target]
	if !ok {
		return false
	}
	_, found := t.Playbook(name)
	return found
}

// RunCycle runs every playbook of every target, in order, and records the
// results. It returns the first error encountered; playbooks after a failure
// still run.
func (a *Ansible) RunCycle(ctx context.Context) error {
	a.mu.RLock()
	targets := make([]config.TargetConfig, 0, len(a.targets))
	for _, t := range a.targets {
		targets = append(targets, t)
	}
	a.mu.RUnlock()
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })

	var firstErr error
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.runTarget(ctx, t, &firstErr); err != nil {
			a.logger.Error("target cycle failed", zap.String("target", t.Name), zap.Error(err))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Trigger runs one named job of a target now. It blocks until the run
// finishes. Runs dispatched through different workers may overlap with an
// in-progress cycle.
func (a *Ansible) Trigger(ctx context.Context, target, name string) error {
	a.mu.RLock()
	t, ok := a.targets[target]
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("unknown target %q", target)
	}
	pb, ok := t.Playbook(name)
	if !ok {
		return fmt.Errorf("unknown playbook %q for target %q", name, target)
	}
	return a.runTarget(ctx, t, nil, pb)
}

// runTarget runs the given target's playbooks (or just one, when specified)
// and records the results. The target is a snapshot, safe to run without
// holding a.mu. It returns the first error encountered; playbooks after a
// failure still run.
func (a *Ansible) runTarget(ctx context.Context, t config.TargetConfig, firstErr *error, only ...config.PlaybookConfig) error {
	playbooks := t.Playbooks
	if len(only) > 0 {
		playbooks = only
	}

	var targetErr error
	for _, pb := range playbooks {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The worker's bridge carries no scope information, so tell it which
		// SSH manager should serve this job.
		sshScope := ""
		if pb.Options.SSH.Enabled {
			sshScope = pb.SSHKey()
		}
		inv, invHosts := a.loadInventory(pb)
		a.logger.Info("running playbook",
			zap.String("target", t.Name),
			zap.String("playbook", pb.Name),
			zap.String("file", pb.PlaybookPath()),
		)
		if err := a.runPlaybook(ctx, pb, sshScope, scopeNames(inv, pb.PlaybookPath()), invHosts); err != nil {
			a.logger.Error("playbook failed",
				zap.String("target", t.Name),
				zap.String("playbook", pb.Name),
				zap.Error(err),
			)
			if targetErr == nil {
				targetErr = err
			}
		}
	}
	if targetErr != nil && firstErr != nil && *firstErr == nil {
		*firstErr = targetErr
	}
	return targetErr
}

// runPlaybook dispatches one playbook to the worker pool, parses the output
// and persists the results.
func (a *Ansible) runPlaybook(ctx context.Context, pb config.PlaybookConfig, sshScope string, scope []string, invHosts []store.InventoryHost) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	started := time.Now().UTC()
	exitCode, output, runErr := a.exec.RunPlaybook(ctx, sshScope, pb.WorkingDir, pb.Args()...)
	finished := time.Now().UTC()

	if runErr != nil {
		if output != "" {
			output += "\n"
		}
		output += fmt.Sprintf("[strangle] runner error: %v", runErr)
	}

	summaries, tasks := parser.Parse(output)

	run := store.RunRecord{
		Target:       pb.TargetName,
		Playbook:     pb.Name,
		StartedAt:    started,
		FinishedAt:   finished,
		DurationMS:   finished.Sub(started).Milliseconds(),
		ExitCode:     exitCode,
		Success:      runErr == nil && exitCode == 0,
		Hosts:        len(summaries),
		ChangedHosts: countStatus(summaries, "changed"),
		FailedHosts:  countStatus(summaries, "failed"),
		Output:       output,
	}

	hostRuns := make([]store.HostRunRecord, 0, len(summaries))
	for _, s := range summaries {
		hostRuns = append(hostRuns, store.HostRunRecord{
			Target:      pb.TargetName,
			Host:        s.Host,
			Status:      parser.Status(s.Ok, s.Changed, s.Failed, s.Unreachable),
			Ok:          s.Ok,
			Changed:     s.Changed,
			Failed:      s.Failed,
			Unreachable: s.Unreachable,
			Skipped:     s.Skipped,
			Rescued:     s.Rescued,
			Ignored:     s.Ignored,
		})
	}

	taskStates := make([]store.TaskStateRecord, 0, len(tasks))
	for _, ts := range tasks {
		taskStates = append(taskStates, store.TaskStateRecord{
			Target:    pb.TargetName,
			Host:      ts.Host,
			Task:      ts.Task,
			Status:    ts.Status,
			LastRunAt: finished,
			LastDiff:  ts.Diff,
			LastError: ts.Error,
		})
	}

	// The run has completed: persist its results even if the context was
	// canceled while it was running (e.g. the process is shutting down).
	if err := a.store.SaveRun(context.WithoutCancel(ctx), store.RunInput{
		Run:        run,
		Hosts:      hostRuns,
		Tasks:      taskStates,
		ScopeNames: scope,
		Inventory:  invHosts,
	}); err != nil {
		return fmt.Errorf("save run: %w", err)
	}

	a.logger.Info("playbook recorded",
		zap.String("target", pb.TargetName),
		zap.String("playbook", pb.Name),
		zap.Int("exit_code", exitCode),
		zap.Bool("success", run.Success),
		zap.Int("hosts", run.Hosts),
		zap.Int64("duration_ms", run.DurationMS),
	)

	if runErr != nil {
		return runErr
	}
	if exitCode != 0 {
		return fmt.Errorf("ansible-playbook exited with code %d", exitCode)
	}
	return nil
}

// loadInventory parses one playbook's inventory directory. It returns nil for
// anything it cannot parse (e.g. an INI file), in which case host membership
// is not tracked for that cycle.
func (a *Ansible) loadInventory(pb config.PlaybookConfig) (*inventory.Inventory, []store.InventoryHost) {
	dir := pb.InventoryPath()
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil, nil
	}
	inv, err := inventory.Load(dir)
	if err != nil {
		a.logger.Warn("inventory load failed",
			zap.String("target", pb.TargetName),
			zap.String("playbook", pb.Name),
			zap.Error(err),
		)
		return nil, nil
	}
	if len(inv.Hosts) == 0 {
		return nil, nil
	}
	hosts := make([]store.InventoryHost, 0, len(inv.Hosts))
	for _, h := range inv.Hosts {
		hosts = append(hosts, store.InventoryHost{
			Name:   h.Name,
			IP:     h.IP,
			User:   h.User,
			Groups: h.Groups,
		})
	}
	return inv, hosts
}

// scopeNames resolves the hosts a playbook is expected to reach by parsing
// the play targets and matching them against the inventory.
func scopeNames(inv *inventory.Inventory, playbookPath string) []string {
	if inv == nil {
		return nil
	}
	patterns, err := playHosts(playbookPath)
	if err != nil || len(patterns) == 0 {
		// Unknown target: assume the whole inventory so no host is
		// incorrectly marked as disappeared.
		names := make([]string, 0, len(inv.Hosts))
		for _, h := range inv.Hosts {
			names = append(names, h.Name)
		}
		return names
	}
	return inv.Members(patterns...)
}

// playHosts reads a playbook file and collects the "hosts" pattern of every
// play.
func playHosts(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var plays []struct {
		Hosts yaml.Node `yaml:"hosts"`
	}
	if err := yaml.Unmarshal(data, &plays); err != nil {
		return nil, err
	}
	var patterns []string
	for _, p := range plays {
		switch p.Hosts.Kind {
		case yaml.ScalarNode:
			patterns = append(patterns, p.Hosts.Value)
		case yaml.SequenceNode:
			for _, n := range p.Hosts.Content {
				if n.Kind == yaml.ScalarNode {
					patterns = append(patterns, n.Value)
				}
			}
		}
	}
	return patterns, nil
}

func countStatus(summaries []parser.HostSummary, status string) int {
	n := 0
	for _, s := range summaries {
		if parser.Status(s.Ok, s.Changed, s.Failed, s.Unreachable) == status {
			n++
		}
	}
	return n
}
