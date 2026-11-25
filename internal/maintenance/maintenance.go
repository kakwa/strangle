// Package maintenance holds the master's periodic background passes: run
// retention and stale-host cleanup. Both use the store directly, so they work
// with PostgreSQL and with the in-memory fallback.
package maintenance

import (
	"context"
	"time"
)

// Task runs a maintenance pass once at startup and then on an interval until
// ctx is done. A nil Task is a no-op, so callers need no enabled check.
type Task struct {
	interval time.Duration
	pass     func(context.Context)
}

func newTask(interval time.Duration, enabled bool, pass func(context.Context)) *Task {
	if !enabled {
		return nil
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &Task{interval: interval, pass: pass}
}

// Run performs one pass immediately, then one per interval until ctx ends.
func (t *Task) Run(ctx context.Context) {
	if t == nil {
		return
	}
	t.Once(ctx)
	ticker := time.NewTicker(t.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.Once(ctx)
		}
	}
}

// Once performs a single pass.
func (t *Task) Once(ctx context.Context) {
	if t == nil {
		return
	}
	t.pass(ctx)
}

// Start runs task in the background and returns a channel closed when it
// exits. A nil task yields an already-closed channel.
func Start(ctx context.Context, task *Task) <-chan struct{} {
	done := make(chan struct{})
	if task == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		task.Run(ctx)
	}()
	return done
}
