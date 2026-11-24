// Package scheduler runs the applier periodically over a set of jobs
// and records the reports in the store.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/store"
)

// Scheduler applies to a fixed list of jobs on a fixed interval.
type Scheduler struct {
	interval time.Duration
	jobs     []applier.Job
	applier  applier.Applier
	store    *store.Store
	logger   *slog.Logger
}

// New returns a Scheduler.
func New(interval time.Duration, jobs []applier.Job, applier applier.Applier, store *store.Store, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		interval: interval,
		jobs:     jobs,
		applier:  applier,
		store:    store,
		logger:   logger,
	}
}

// Run applies to all jobs immediately, then on every tick of the interval,
// until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()

	s.runOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx)
		}
	}
}

func (s *Scheduler) runOnce(ctx context.Context) {
	s.logger.Info("run started", "jobs", len(s.jobs))
	for _, job := range s.jobs {
		report, err := s.applier.Apply(ctx, job)
		if err != nil {
			s.logger.Error("apply failed", "job", job.ID, "error", err)
			report = applier.Report{
				JobID:     job.ID,
				JobTitle:  job.Title,
				Company:   job.Company,
				AppliedAt: time.Now().UTC(),
				ExitCode:  -1,
				Success:   false,
				Message:   err.Error(),
			}
		}
		s.store.Add(report)
	}
	s.logger.Info("run finished", "jobs", len(s.jobs))
}
