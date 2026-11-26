package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/model"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"go.uber.org/zap"
)

func TestMaintenance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Maintenance Suite")
}

type cleanupCall struct {
	target string
	delay  time.Duration
}

type stubStore struct {
	model.Store
	calls []cleanupCall
}

func (s *stubStore) PruneHosts(_ context.Context, target string, delay time.Duration) (int64, error) {
	s.calls = append(s.calls, cleanupCall{target: target, delay: delay})
	return 1, nil
}

var _ = Describe("HostCleanup", func() {
	It("applies per-collection delay over the server default", func() {
		st := &stubStore{}
		targets := []cfg.TargetConfig{
			{Name: "home"},
			{Name: "edge", HostCleanup: cfg.HostCleanupConfig{Delay: 6 * time.Hour}},
		}
		m := NewHostCleanup(st, cfg.HostCleanupConfig{Delay: 24 * time.Hour, Interval: time.Minute},
			func() []cfg.TargetConfig { return targets }, zap.NewNop())
		Expect(m).NotTo(BeNil())
		m.Once(context.Background())

		Expect(st.calls).To(HaveLen(2))
		Expect(st.calls[0]).To(Equal(cleanupCall{"home", 24 * time.Hour}))
		Expect(st.calls[1]).To(Equal(cleanupCall{"edge", 6 * time.Hour}))
	})

	It("is disabled when no delay is configured", func() {
		st := &stubStore{}
		targets := []cfg.TargetConfig{{Name: "home"}}
		Expect(NewHostCleanup(st, cfg.HostCleanupConfig{}, func() []cfg.TargetConfig { return targets }, zap.NewNop())).To(BeNil())

		var m *Task
		m.Once(context.Background())
		m.Run(context.Background())
		Expect(st.calls).To(BeEmpty())
	})
})
