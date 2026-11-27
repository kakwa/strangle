package maintenance

import (
	"context"
	"sync/atomic"
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

var _ = Describe("Task", func() {
	It("runs one pass immediately and one per interval until cancelled", func() {
		var calls int32
		t := newTask(10*time.Millisecond, true, func(context.Context) {
			atomic.AddInt32(&calls, 1)
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := Start(ctx, t)

		deadline := time.Now().Add(3 * time.Second)
		for atomic.LoadInt32(&calls) < 3 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		cancel()
		<-done
		Expect(atomic.LoadInt32(&calls)).To(BeNumerically(">=", 3))
	})

	It("returns immediately when the context is already cancelled", func() {
		var calls int32
		t := newTask(time.Hour, true, func(context.Context) {
			atomic.AddInt32(&calls, 1)
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		t.Run(ctx)
		Expect(atomic.LoadInt32(&calls)).To(Equal(int32(1)))
	})

	It("Once performs exactly one pass", func() {
		var calls int32
		t := newTask(time.Hour, true, func(context.Context) {
			atomic.AddInt32(&calls, 1)
		})
		t.Once(context.Background())
		Expect(atomic.LoadInt32(&calls)).To(Equal(int32(1)))
	})

	It("Start yields a closed channel for a nil task", func() {
		Expect(Start(context.Background(), nil)).To(BeClosed())
	})

	It("newTask returns nil when the pass is disabled", func() {
		Expect(newTask(time.Minute, false, nil)).To(BeNil())
	})
})
