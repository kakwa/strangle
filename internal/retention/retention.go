// Package retention prunes old run history in the background. It is a
// master-process maintenance task: it uses the store directly, so it works
// with both the PostgreSQL store and the in-memory fallback.
package retention

import (
	"context"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
)

// Manager runs retention passes for every configured collection.
type Manager struct {
	store    store.Store
	server   config.RetentionConfig
	targets  func() []config.TargetConfig
	interval time.Duration
	logger   *zap.Logger
}

// New returns a manager when at least one retention rule is enabled, or nil
// when retention is disabled. targets is a provider rather than a snapshot so
// a git-backed collection's reloaded policy is picked up on the next pass.
func New(st store.Store, cfg config.RetentionConfig, targets func() []config.TargetConfig, logger *zap.Logger) *Manager {
	enabled := cfg.Enabled()
	for _, t := range targets() {
		if t.Retention.Enabled() {
			enabled = true
			break
		}
	}
	if !enabled {
		return nil
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	return &Manager{
		store:    st,
		server:   cfg,
		targets:  targets,
		interval: interval,
		logger:   logger,
	}
}

// Run prunes once at startup and then on each interval until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	if m == nil {
		return
	}
	m.Prune(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Prune(ctx)
		}
	}
}

// Prune applies one pass to every collection. Individual failures are logged
// and do not stop the other collections or the maintenance loop.
func (m *Manager) Prune(ctx context.Context) {
	if m == nil {
		return
	}
	for _, target := range m.targets() {
		policy := store.RetentionPolicy{
			MaxAge:   m.server.MaxAge,
			MaxCount: m.server.MaxCount,
		}
		if target.Retention.Enabled() {
			policy.MaxAge = target.Retention.MaxAge
			policy.MaxCount = target.Retention.MaxCount
		}
		if !policy.Enabled() {
			continue
		}
		res, err := m.store.PruneRuns(ctx, target.Name, policy)
		if err != nil {
			m.logger.Warn("run retention failed",
				zap.String("collection", target.Name), zap.Error(err))
			continue
		}
		if res.Runs > 0 {
			m.logger.Info("purged old runs",
				zap.String("collection", target.Name),
				zap.Int64("runs", res.Runs),
				zap.Int64("host_runs", res.HostRuns),
				zap.Int64("task_results", res.TaskResults))
		}
	}
}
