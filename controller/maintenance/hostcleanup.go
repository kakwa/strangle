package maintenance

import (
	"context"

	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/model"
	"go.uber.org/zap"
)

// NewHostCleanup returns a task that removes stale hosts, or nil when host
// cleanup is disabled. targets is a provider rather than a snapshot so a
// git-backed collection's reloaded policy is picked up on the next pass.
func NewHostCleanup(st model.Store, conf cfg.HostCleanupConfig, targets func() []cfg.TargetConfig, logger *zap.Logger) *Task {
	enabled := conf.Enabled()
	for _, t := range targets() {
		if t.HostCleanup.Enabled() {
			enabled = true
			break
		}
	}
	return newTask(conf.Interval, enabled, func(ctx context.Context) {
		for _, target := range targets() {
			delay := conf.Delay
			if target.HostCleanup.Enabled() {
				delay = target.HostCleanup.Delay
			}
			if delay <= 0 {
				continue
			}
			removed, err := st.PruneHosts(ctx, target.Name, delay)
			if err != nil {
				logger.Warn("host cleanup failed",
					zap.String("collection", target.Name), zap.Error(err))
				continue
			}
			if removed > 0 {
				logger.Info("removed stale hosts",
					zap.String("collection", target.Name), zap.Int64("hosts", removed))
			}
		}
	})
}
