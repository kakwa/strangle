package applier

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kakwa/strangle/internal/ansible"
	"github.com/kakwa/strangle/internal/config"
)

// Ansible applies jobs by running an embedded ansible-playbook with --diff.
type Ansible struct {
	cfg    config.AnsibleConfig
	runner *ansible.Runner
	logger *slog.Logger
}

// NewAnsible returns an Applier backed by an embedded Ansible interpreter.
func NewAnsible(cfg config.AnsibleConfig, runner *ansible.Runner, logger *slog.Logger) *Ansible {
	return &Ansible{cfg: cfg, runner: runner, logger: logger}
}

// Apply runs the configured playbook and records the exit code and --diff output.
func (a *Ansible) Apply(ctx context.Context, job Job) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}

	result, err := a.runner.Run(a.cfg.Args()...)
	report := Report{
		JobID:     job.ID,
		JobTitle:  job.Title,
		Company:   job.Company,
		AppliedAt: time.Now().UTC(),
		ExitCode:  result.ExitCode,
		Success:   err == nil && result.ExitCode == 0,
		Message:   result.Output,
	}
	if err != nil {
		return report, fmt.Errorf("ansible-playbook: %w: %s", err, result.Output)
	}

	a.logger.Info("applied to job",
		"job", job.ID,
		"company", job.Company,
		"exit_code", result.ExitCode,
	)
	return report, nil
}
