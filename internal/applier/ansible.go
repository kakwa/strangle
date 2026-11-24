// Package applier runs Ansible playbooks on a schedule and records their
// results: run metadata, per-host recap lines and per-task change traces.
package applier

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/kakwa/strangle/internal/ansible"
	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/inventory"
	"github.com/kakwa/strangle/internal/parser"
	"github.com/kakwa/strangle/internal/store"
	"gopkg.in/yaml.v3"
)

// Ansible applies playbooks with the embedded ansible-playbook and records
// the results in a store.
type Ansible struct {
	cfg    config.AnsibleConfig
	runner *ansible.Runner
	store  store.Store
	logger *slog.Logger

	// mu serializes playbook runs: the embedded interpreter is
	// single-threaded and the working directory is process-wide.
	mu sync.Mutex
}

// NewAnsible returns an Applier backed by an embedded Ansible interpreter.
func NewAnsible(cfg config.AnsibleConfig, runner *ansible.Runner, st store.Store, logger *slog.Logger) *Ansible {
	return &Ansible{cfg: cfg, runner: runner, store: st, logger: logger}
}

// Playbooks returns the configured playbook list.
func (a *Ansible) Playbooks() []string {
	return a.cfg.PlaybookList()
}

// KnownPlaybook reports whether playbook is in the configured list.
func (a *Ansible) KnownPlaybook(playbook string) bool {
	for _, p := range a.cfg.PlaybookList() {
		if p == playbook {
			return true
		}
	}
	return false
}

// RunCycle runs every configured playbook sequentially and records the
// results. It returns the first error encountered; playbooks after a failure
// still run.
func (a *Ansible) RunCycle(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	inv, invHosts := a.loadInventory()

	var firstErr error
	for _, pb := range a.cfg.PlaybookList() {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.logger.Info("running playbook", "playbook", pb)
		if err := a.runPlaybook(ctx, pb, scopeNames(inv, a.cfg.PlaybookPath(pb)), invHosts); err != nil {
			a.logger.Error("playbook failed", "playbook", pb, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Trigger runs a single configured playbook now. It blocks until the run
// finishes (waiting for any in-progress cycle first).
func (a *Ansible) Trigger(ctx context.Context, playbook string) error {
	if !a.KnownPlaybook(playbook) {
		return fmt.Errorf("unknown playbook %q", playbook)
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	inv, invHosts := a.loadInventory()
	return a.runPlaybook(ctx, playbook, scopeNames(inv, a.cfg.PlaybookPath(playbook)), invHosts)
}

// runPlaybook executes one playbook, parses the output and persists the
// results. The caller must hold a.mu.
func (a *Ansible) runPlaybook(ctx context.Context, playbook string, scope []string, invHosts []store.InventoryHost) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	started := time.Now().UTC()
	result, runErr := a.runner.RunDir(a.cfg.WorkingDir, a.cfg.Args(playbook)...)
	finished := time.Now().UTC()

	output := result.Output
	if runErr != nil {
		if output != "" {
			output += "\n"
		}
		output += fmt.Sprintf("[strangle] runner error: %v", runErr)
	}

	summaries, tasks := parser.Parse(output)

	run := store.RunRecord{
		Playbook:     playbook,
		StartedAt:    started,
		FinishedAt:   finished,
		DurationMS:   finished.Sub(started).Milliseconds(),
		ExitCode:     result.ExitCode,
		Success:      runErr == nil && result.ExitCode == 0,
		Hosts:        len(summaries),
		ChangedHosts: countStatus(summaries, "changed"),
		FailedHosts:  countStatus(summaries, "failed"),
		Output:       output,
	}

	hostRuns := make([]store.HostRunRecord, 0, len(summaries))
	for _, s := range summaries {
		hostRuns = append(hostRuns, store.HostRunRecord{
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
	for _, t := range tasks {
		taskStates = append(taskStates, store.TaskStateRecord{
			Host:      t.Host,
			Task:      t.Task,
			Status:    t.Status,
			LastRunAt: finished,
			LastDiff:  t.Diff,
			LastError: t.Error,
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
		"playbook", playbook,
		"exit_code", result.ExitCode,
		"success", run.Success,
		"hosts", run.Hosts,
		"duration_ms", run.DurationMS,
	)

	if runErr != nil {
		return runErr
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("ansible-playbook exited with code %d", result.ExitCode)
	}
	return nil
}

// loadInventory parses the inventory directory. It returns nil for anything
// it cannot parse (e.g. an INI file), in which case host membership is not
// tracked for that cycle.
func (a *Ansible) loadInventory() (*inventory.Inventory, []store.InventoryHost) {
	dir := a.cfg.InventoryPath()
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil, nil
	}
	inv, err := inventory.Load(dir)
	if err != nil {
		a.logger.Warn("inventory load failed", "error", err)
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
