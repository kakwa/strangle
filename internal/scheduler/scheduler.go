// Package scheduler starts playbook runs according to each job's schedule.
//
// A job with an explicit duration or cron schedule fires on that schedule; a
// job without one keeps the original behaviour of running immediately at
// startup and then on server.yml's global interval. The live target list is
// reconciled periodically, so a git sync can add, remove or reschedule jobs
// without a restart.
package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/kakwa/strangle/internal/schedule"
	"go.uber.org/zap"
)

// Job is one scheduled (target, playbook) pair. An empty Schedule means the
// server-level fallback interval applies.
type Job struct {
	Target   string
	Playbook string
	Schedule string
}

// JobProvider returns the live list of jobs. It is called on every reconcile
// tick so git-backed targets can change their schedules at runtime.
type JobProvider func() []Job

// RunFunc starts one playbook run. With a database it enqueues a River job;
// without one it starts a goroutine in the master.
type RunFunc func(ctx context.Context, target, playbook string) error

// Scheduler owns one timer per job.
type Scheduler struct {
	fallback          time.Duration
	provider          JobProvider
	run               RunFunc
	logger            *zap.Logger
	reconcileInterval time.Duration

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	spec   string
	cancel context.CancelFunc
}

// New returns a Scheduler. fallback is server.yml's interval for jobs that do
// not define their own schedule.
func New(fallback time.Duration, provider JobProvider, run RunFunc, logger *zap.Logger) *Scheduler {
	if provider == nil {
		provider = func() []Job { return nil }
	}
	if run == nil {
		run = func(context.Context, string, string) error { return errors.New("no scheduled run function configured") }
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Scheduler{
		fallback:          fallback,
		provider:          provider,
		run:               run,
		logger:            logger,
		reconcileInterval: 10 * time.Second,
		entries:           map[string]*entry{},
	}
}

// Run starts the initial set of jobs and keeps it in sync until ctx is
// canceled.
func (s *Scheduler) Run(ctx context.Context) {
	s.reconcile(ctx)
	interval := s.reconcileInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.stopAll()
			return
		case <-ticker.C:
			s.reconcile(ctx)
		}
	}
}

// reconcile starts new jobs, stops removed ones, and restarts jobs whose
// schedule changed. It is safe to call repeatedly.
func (s *Scheduler) reconcile(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	desired := make(map[string]Job)
	for _, job := range s.provider() {
		key := jobKey(job)
		if _, duplicate := desired[key]; duplicate {
			s.logger.Warn("duplicate scheduled job ignored",
				zap.String("target", job.Target), zap.String("playbook", job.Playbook))
			continue
		}
		desired[key] = job
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for key, e := range s.entries {
		job, stillWanted := desired[key]
		if !stillWanted || job.Schedule != e.spec {
			e.cancel()
			delete(s.entries, key)
		}
	}
	for key, job := range desired {
		if _, running := s.entries[key]; running {
			continue
		}
		sch, startup, err := s.resolve(job.Schedule)
		if err != nil {
			s.logger.Warn("invalid job schedule",
				zap.String("target", job.Target),
				zap.String("playbook", job.Playbook),
				zap.String("schedule", job.Schedule),
				zap.Error(err))
			continue
		}
		if !sch.Enabled() {
			continue // manual runs only
		}
		jobCtx, cancel := context.WithCancel(ctx)
		s.entries[key] = &entry{spec: job.Schedule, cancel: cancel}
		go s.runJob(jobCtx, job, sch, startup)
	}
}

// resolve turns a job's schedule string into a Schedule. Empty schedules use
// the fallback interval and keep the legacy startup run.
func (s *Scheduler) resolve(spec string) (schedule.Schedule, bool, error) {
	if strings.TrimSpace(spec) == "" {
		if s.fallback <= 0 {
			return nil, false, errors.New("no default interval configured")
		}
		sch, err := schedule.Parse(s.fallback.String())
		return sch, true, err
	}
	sch, err := schedule.Parse(spec)
	return sch, false, err
}

func (s *Scheduler) runJob(ctx context.Context, job Job, sch schedule.Schedule, startup bool) {
	next := time.Now()
	if !startup {
		// Cron-like schedules wait for their first matching time; only the
		// legacy global interval runs immediately at startup.
		next = sch.Next(next)
		if next.IsZero() {
			return
		}
	}
	for {
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.execute(ctx, job)
		next = sch.Next(time.Now())
		if next.IsZero() {
			return
		}
	}
}

func (s *Scheduler) execute(ctx context.Context, job Job) {
	if err := ctx.Err(); err != nil {
		return
	}
	started := time.Now()
	s.logger.Info("scheduled run started",
		zap.String("target", job.Target),
		zap.String("playbook", job.Playbook),
		zap.String("schedule", scheduleLabel(job.Schedule)))
	err := s.run(ctx, job.Target, job.Playbook)
	if err != nil && errors.Is(err, context.Canceled) {
		return
	}
	if err != nil {
		s.logger.Error("scheduled run failed",
			zap.String("target", job.Target),
			zap.String("playbook", job.Playbook),
			zap.Error(err))
	}
	s.logger.Info("scheduled run finished",
		zap.String("target", job.Target),
		zap.String("playbook", job.Playbook),
		zap.Duration("duration", time.Since(started).Round(time.Millisecond)))
}

func (s *Scheduler) stopAll() {
	s.mu.Lock()
	entries := s.entries
	s.entries = map[string]*entry{}
	s.mu.Unlock()
	for _, e := range entries {
		e.cancel()
	}
}

func jobKey(job Job) string { return job.Target + "/" + job.Playbook }

func scheduleLabel(spec string) string {
	if spec == "" {
		return "default"
	}
	return spec
}
