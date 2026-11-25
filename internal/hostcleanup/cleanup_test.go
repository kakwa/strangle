package hostcleanup

import (
	"context"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
)

type cleanupCall struct {
	target string
	delay  time.Duration
}

type stubStore struct {
	store.Store
	calls []cleanupCall
}

func (s *stubStore) PruneHosts(_ context.Context, target string, delay time.Duration) (int64, error) {
	s.calls = append(s.calls, cleanupCall{target: target, delay: delay})
	return 1, nil
}

func TestManagerCleanPolicies(t *testing.T) {
	st := &stubStore{}
	targets := []config.TargetConfig{
		{Name: "home"},
		{Name: "edge", HostCleanup: config.HostCleanupConfig{Delay: 6 * time.Hour}},
	}
	m := New(st, config.HostCleanupConfig{Delay: 24 * time.Hour, Interval: time.Minute},
		func() []config.TargetConfig { return targets }, zap.NewNop())
	if m == nil {
		t.Fatal("New() = nil, want an enabled manager")
	}
	m.Clean(context.Background())

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

func TestManagerDisabled(t *testing.T) {
	st := &stubStore{}
	targets := []config.TargetConfig{{Name: "home"}}
	if m := New(st, config.HostCleanupConfig{}, func() []config.TargetConfig { return targets }, zap.NewNop()); m != nil {
		t.Fatal("New() enabled cleanup with no delay")
	}
	var m *Manager
	m.Clean(context.Background())
	m.Run(context.Background())
	if len(st.calls) != 0 {
		t.Errorf("disabled manager called PruneHosts: %v", st.calls)
	}
}
