package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestDuplicateJobsAreIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counter := newCallCounter()
	job := Job{Target: "dup", Playbook: "pb", Schedule: "@every 1h"}
	s := New(time.Hour, func() []Job { return []Job{job, job} }, counter.run, zap.NewNop())

	s.reconcile(ctx)
	s.mu.Lock()
	entries := len(s.entries)
	s.mu.Unlock()
	if entries != 1 {
		t.Errorf("entries = %d, want 1 despite the duplicate job", entries)
	}
	s.stopAll()
}

func TestInvalidScheduleIsSkipped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counter := newCallCounter()
	s := New(time.Hour, func() []Job {
		return []Job{{Target: "bad", Playbook: "pb", Schedule: "not-a-schedule"}}
	}, counter.run, zap.NewNop())

	s.reconcile(ctx)
	s.mu.Lock()
	entries := len(s.entries)
	s.mu.Unlock()
	if entries != 0 {
		t.Errorf("entries = %d, want 0 for an invalid schedule", entries)
	}
	if got := counter.count("bad/pb"); got != 0 {
		t.Errorf("invalid job ran %d times", got)
	}
}

func TestScheduleChangeRestartsJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counter := newCallCounter()
	var mu sync.Mutex
	schedule := "@every 1h"
	s := New(time.Hour, func() []Job {
		mu.Lock()
		defer mu.Unlock()
		return []Job{{Target: "changing", Playbook: "pb", Schedule: schedule}}
	}, counter.run, zap.NewNop())

	setSchedule := func(next string) {
		mu.Lock()
		schedule = next
		mu.Unlock()
		s.reconcile(ctx)
	}
	spec := func() string {
		s.mu.Lock()
		defer s.mu.Unlock()
		if e, ok := s.entries["changing/pb"]; ok {
			return e.spec
		}
		return ""
	}

	s.reconcile(ctx)
	if got := spec(); got != "@every 1h" {
		t.Fatalf("initial schedule = %q, want @every 1h", got)
	}
	setSchedule("@every 20ms")
	if got := spec(); got != "@every 20ms" {
		t.Fatalf("schedule after change = %q, want @every 20ms", got)
	}
	waitFor(t, "the rescheduled job to run", func() bool { return counter.count("changing/pb") >= 1 })

	setSchedule("@off")
	if got := spec(); got != "" {
		t.Fatalf("schedule after disabling = %q, want no entry", got)
	}
}
