package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type callCounter struct {
	mu    sync.Mutex
	calls map[string]int
}

func newCallCounter() *callCounter { return &callCounter{calls: map[string]int{}} }

func (c *callCounter) run(_ context.Context, target, playbook string) error {
	c.mu.Lock()
	c.calls[target+"/"+playbook]++
	c.mu.Unlock()
	return nil
}

func (c *callCounter) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[key]
}

func waitFor(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startScheduler(t *testing.T, s *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("scheduler did not stop after cancellation")
		}
	})
}

func TestExplicitSchedulesAreIndependent(t *testing.T) {
	counter := newCallCounter()
	s := New(time.Hour, func() []Job {
		return []Job{
			{Target: "fast", Playbook: "pb", Schedule: "@every 20ms"},
			{Target: "slow", Playbook: "pb", Schedule: "@every 2s"},
			{Target: "off", Playbook: "pb", Schedule: "@off"},
		}
	}, counter.run, zap.NewNop())
	// Tests can inspect the unexported reconcile interval because they live
	// in the same package; production uses the 10-second default.
	s.reconcileInterval = 10 * time.Millisecond
	startScheduler(t, s)

	waitFor(t, "the fast job to run 3 times", func() bool { return counter.count("fast/pb") >= 3 })
	if got := counter.count("off/pb"); got != 0 {
		t.Errorf("disabled job ran %d times, want 0", got)
	}
	if got := counter.count("slow/pb"); got != 0 {
		t.Errorf("slow job ran %d times before its first tick, want 0", got)
	}
}

func TestFallbackRunsImmediately(t *testing.T) {
	counter := newCallCounter()
	s := New(20*time.Millisecond, func() []Job {
		return []Job{{Target: "legacy", Playbook: "pb"}} // no explicit schedule
	}, counter.run, zap.NewNop())
	s.reconcileInterval = 10 * time.Millisecond
	startScheduler(t, s)

	waitFor(t, "the fallback job to run twice", func() bool { return counter.count("legacy/pb") >= 2 })
}

func TestLiveJobListIsReconciled(t *testing.T) {
	counter := newCallCounter()
	var mu sync.Mutex
	jobs := []Job{{Target: "old", Playbook: "pb", Schedule: "@every 20ms"}}
	provider := func() []Job {
		mu.Lock()
		defer mu.Unlock()
		return append([]Job(nil), jobs...)
	}
	setJobs := func(next ...Job) {
		mu.Lock()
		jobs = next
		mu.Unlock()
	}

	s := New(time.Hour, provider, counter.run, zap.NewNop())
	s.reconcileInterval = 10 * time.Millisecond
	startScheduler(t, s)
	waitFor(t, "the old job to run", func() bool { return counter.count("old/pb") >= 1 })

	setJobs(Job{Target: "new", Playbook: "pb", Schedule: "@every 20ms"})
	waitFor(t, "the new job to run", func() bool { return counter.count("new/pb") >= 1 })
	oldAtSwitch := counter.count("old/pb")
	time.Sleep(100 * time.Millisecond)
	if got := counter.count("old/pb"); got != oldAtSwitch {
		t.Errorf("removed job ran %d more times (from %d to %d)", got-oldAtSwitch, oldAtSwitch, got)
	}
}
