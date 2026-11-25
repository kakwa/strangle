package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/cfg"
	"github.com/kakwa/strangle/internal/db"
	"go.uber.org/zap"
)

type cleanupCall struct {
	target string
	delay  time.Duration
}

type stubStore struct {
	db.Store
	calls []cleanupCall
}

func (s *stubStore) PruneHosts(_ context.Context, target string, delay time.Duration) (int64, error) {
	s.calls = append(s.calls, cleanupCall{target: target, delay: delay})
	return 1, nil
}

func TestHostCleanupPolicies(t *testing.T) {
	st := &stubStore{}
	targets := []cfg.TargetConfig{
		{Name: "home"},
		{Name: "edge", HostCleanup: cfg.HostCleanupConfig{Delay: 6 * time.Hour}},
	}
	m := NewHostCleanup(st, cfg.HostCleanupConfig{Delay: 24 * time.Hour, Interval: time.Minute},
		func() []cfg.TargetConfig { return targets }, zap.NewNop())
	if m == nil {
		t.Fatal("NewHostCleanup() = nil, want an enabled task")
	}
	m.Once(context.Background())

	if len(st.calls) != 2 {
		t.Fatalf("PruneHosts calls = %v, want two", st.calls)
	}
	if st.calls[0] != (cleanupCall{"home", 24 * time.Hour}) {
		t.Errorf("home call = %+v, want the server delay", st.calls[0])
	}
	if st.calls[1] != (cleanupCall{"edge", 6 * time.Hour}) {
		t.Errorf("edge call = %+v, want the collection delay", st.calls[1])
	}
}

func TestHostCleanupDisabled(t *testing.T) {
	st := &stubStore{}
	targets := []cfg.TargetConfig{{Name: "home"}}
	if m := NewHostCleanup(st, cfg.HostCleanupConfig{}, func() []cfg.TargetConfig { return targets }, zap.NewNop()); m != nil {
		t.Fatal("NewHostCleanup() enabled cleanup with no delay")
	}
	var m *Task
	m.Once(context.Background())
	m.Run(context.Background())
	if len(st.calls) != 0 {
		t.Errorf("disabled task called PruneHosts: %v", st.calls)
	}
}
