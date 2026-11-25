package maintenance

import (
	"context"

	"github.com/kakwa/strangle/internal/cfg"
	"github.com/kakwa/strangle/internal/db"
	"go.uber.org/zap"
)

// NewRetention returns a task that prunes old runs, or nil when retention is
// disabled. targets is a provider rather than a snapshot so a git-backed
// collection's reloaded policy is picked up on the next pass.
func NewRetention(st db.Store, conf cfg.RetentionConfig, targets func() []cfg.TargetConfig, logger *zap.Logger) *Task {
	enabled := conf.Enabled()
	for _, t := range targets() {
		if t.Retention.Enabled() {
			enabled = true
			break
		}
	}
	return newTask(conf.Interval, enabled, func(ctx context.Context) {
		for _, target := range targets() {
			policy := db.RetentionPolicy{
				MaxAge:   conf.MaxAge,
				MaxCount: conf.MaxCount,
			}
			if target.Retention.Enabled() {
				policy.MaxAge = target.Retention.MaxAge
				policy.MaxCount = target.Retention.MaxCount
			}
			if !policy.Enabled() {
				continue
			}
			res, err := st.PruneRuns(ctx, target.Name, policy)
			if err != nil {
				logger.Warn("run retention failed",
					zap.String("collection", target.Name), zap.Error(err))
				continue
			}
			if res.Runs > 0 {
				logger.Info("purged old runs",
					zap.String("collection", target.Name),
					zap.Int64("runs", res.Runs),
					zap.Int64("host_runs", res.HostRuns),
					zap.Int64("task_results", res.TaskResults))
			}
		}
	})
}
