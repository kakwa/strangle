// Package playbook_applier runs Ansible playbooks on a schedule and records their
// results: run metadata, per-host recap lines and per-task change traces.
package playbook_applier

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/store"
	"go.uber.org/zap"
)

// Executor runs one playbook and reports its outcome: the ansible-playbook
// exit code, the captured output, per-host facts and an error for failures
// outside the interpreter (setup, transport or a closed worker). The master's
// worker pool implements it; tests can provide their own.
type Executor interface {
	RunPlaybook(ctx context.Context, collection, scope, workingDir string, args ...string) (int, string, map[string]json.RawMessage, error)
}

type runIDKey struct{}

// WithRunID attaches the store run ID to a run's context so the applier can
// update the record created when the job was enqueued.
func WithRunID(ctx context.Context, id uint64) context.Context {
	return context.WithValue(ctx, runIDKey{}, id)
}

func runIDFrom(ctx context.Context) uint64 {
	id, _ := ctx.Value(runIDKey{}).(uint64)
	return id
}

type checkKey struct{}

// WithCheck marks one run as a check (dry) run. Scheduled runs never set it;
// API/UI callers can request it per run.
func WithCheck(ctx context.Context, check bool) context.Context {
	return context.WithValue(ctx, checkKey{}, check)
}

func checkFrom(ctx context.Context) bool {
	check, _ := ctx.Value(checkKey{}).(bool)
	return check
}

// Ansible applies playbooks for every configured target by dispatching each
// run to a worker process and records the results in a model.
type Ansible struct {
	mu      sync.RWMutex
	targets map[string]cfg.TargetConfig
	exec    Executor
	store   store.Store
	logger  *zap.Logger
}

// NewAnsible returns an Applier for the given targets. Runs are executed by
// the given executor (a pool of worker processes in production).
func NewAnsible(targets []cfg.TargetConfig, exec Executor, st store.Store, logger *zap.Logger) *Ansible {
	a := &Ansible{
		targets: map[string]cfg.TargetConfig{},
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
func (a *Ansible) UpdateTargets(targets []cfg.TargetConfig) {
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

// Target returns the resolved configuration of one target, including its
// organization. The API uses it to expose collections as first-class
// entities under /api/v1/orgs/{org}/collections/{collection}.
func (a *Ansible) Target(name string) (cfg.TargetConfig, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	t, ok := a.targets[name]
	return t, ok
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
func (a *Ansible) PlaybookConfigs(target string) []cfg.PlaybookConfig {
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
	targets := make([]cfg.TargetConfig, 0, len(a.targets))
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
func (a *Ansible) runTarget(ctx context.Context, t cfg.TargetConfig, firstErr *error, only ...cfg.PlaybookConfig) error {
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
		if err := a.runPlaybook(ctx, t, pb, sshScope, scopeNames(inv, pb.PlaybookPath()), invHosts); err != nil {
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
func (a *Ansible) runPlaybook(ctx context.Context, target cfg.TargetConfig, pb cfg.PlaybookConfig, sshScope string, scope []string, invHosts []store.InventoryHost) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	id := runIDFrom(ctx)
	started := time.Now().UTC()
	if id != 0 && a.store != nil {
		record, err := a.store.Run(ctx, id)
		if err == nil {
			record.State = store.RunStateRunning
			record.StartedAt = started
			if err := a.store.UpdateRun(ctx, record); err != nil {
				a.logger.Warn("failed to mark run as running", zap.Uint64("run", id), zap.Error(err))
			}
		} else {
			a.logger.Warn("run record not found for in-flight run", zap.Uint64("run", id), zap.Error(err))
		}
	}
	check := checkFrom(ctx)
	exitCode, output, facts, runErr := a.exec.RunPlaybook(ctx, target.Name, sshScope, pb.WorkingDir, pb.Args(check)...)
	finished := time.Now().UTC()

	if runErr != nil {
		if output != "" {
			output += "\n"
		}
		output += fmt.Sprintf("[strangle] runner error: %v", runErr)
	}

	summaries, tasks := Parse(output)

	state := store.RunStateCompleted
	canceled := false
	if ctx.Err() != nil {
		state = store.RunStateCancelled
		canceled = true
		output += "\n[strangle] run canceled"
	}

	changedHosts, failedHosts := 0, 0
	hostRuns := make([]store.HostRunRecord, 0, len(summaries))
	for _, s := range summaries {
		status := hostStatus(s)
		switch status {
		case "changed":
			changedHosts++
		case "failed":
			failedHosts++
		}
		hostRuns = append(hostRuns, store.HostRunRecord{
			Target:       pb.TargetName,
			Host:         s.Host,
			Status:       status,
			TaskCounters: s.TaskCounters,
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

	run := store.RunRecord{
		ID:           id,
		Target:       pb.TargetName,
		Playbook:     pb.Name,
		StartedAt:    started,
		FinishedAt:   finished,
		DurationMS:   finished.Sub(started).Milliseconds(),
		ExitCode:     exitCode,
		Success:      !canceled && runErr == nil && exitCode == 0,
		Hosts:        len(summaries),
		ChangedHosts: changedHosts,
		FailedHosts:  failedHosts,
		Output:       output,
		State:        state,
		Canceled:     canceled,
		Check:        check,
		Inventory:    pb.Inventory,
	}

	// The run has completed: persist its results even if the context was
	// canceled while it was running (e.g. the process is shutting down).
	if err := a.store.SaveRun(context.WithoutCancel(ctx), store.RunInput{
		Run:   run,
		Hosts: hostRuns,
		Tasks: taskStates,
		Facts: facts,
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

// markRunning updates the store record created when the run was enqueued so
// the API can show it as running. A missing record or store error is only
// logged: the run itself still proceeds.
func (a *Ansible) markRunning(ctx context.Context, id uint64, started time.Time) {
	if id == 0 || a.store == nil {
		return
	}
	record, err := a.store.Run(ctx, id)
	if err != nil {
		a.logger.Warn("run record not found for in-flight run", zap.Uint64("run", id), zap.Error(err))
		return
	}
	record.State = store.RunStateRunning
	record.StartedAt = started
	if err := a.store.UpdateRun(ctx, record); err != nil {
		a.logger.Warn("failed to mark run as running", zap.Uint64("run", id), zap.Error(err))
	}
}

// hostStatus reduces a host's per-status counters to a single outcome.
func hostStatus(s HostSummary) string {
	switch {
	case s.Failed > 0:
		return "failed"
	case s.Unreachable > 0:
		return "unreachable"
	case s.Changed > 0:
		return "changed"
	}
	return "ok"
}
