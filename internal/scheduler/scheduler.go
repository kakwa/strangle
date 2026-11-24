// Package scheduler runs a cycle function periodically.
package scheduler

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

// Scheduler runs the given cycle function immediately, then on every tick of
// the interval, until ctx is canceled.
type Scheduler struct {
	interval time.Duration
	cycle    func(ctx context.Context) error
	logger   *zap.Logger
}

// New returns a Scheduler.
func New(interval time.Duration, cycle func(ctx context.Context) error, logger *zap.Logger) *Scheduler {
	return &Scheduler{interval: interval, cycle: cycle, logger: logger}
}

// Run executes the cycle immediately and on every tick until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) {
	s.runCycle(ctx, "startup")

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runCycle(ctx, "interval")
		}
	}
}

func (s *Scheduler) runCycle(ctx context.Context, trigger string) {
	if err := ctx.Err(); err != nil {
		return
	}
	s.logger.Info("run started", zap.String("trigger", trigger))
	started := time.Now()
	if err := s.cycle(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			s.logger.Info("run canceled", zap.String("trigger", trigger))
			return
		}
		s.logger.Error("run failed", zap.String("trigger", trigger), zap.Error(err))
	}
	s.logger.Info("run finished",
		zap.String("trigger", trigger),
		zap.Duration("duration", time.Since(started).Round(time.Millisecond)),
	)
}
