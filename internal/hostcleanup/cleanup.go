// Package hostcleanup removes stale hosts in the background. It is a
// master-process maintenance task and uses the store directly, so it works
// with both the PostgreSQL store and the in-memory fallback.
package hostcleanup

import (
	"context"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
)

// Manager runs cleanup passes for every configured collection.
type Manager struct {
	store    store.Store
	server   config.HostCleanupConfig
	targets  func() []config.TargetConfig
	interval time.Duration
	logger   *zap.Logger
}

// New returns a manager when at least one collection has a delay configured,
// or nil when host cleanup is disabled. targets is a provider rather than a
// snapshot so a git-backed collection's reloaded policy is picked up.
func New(st store.Store, cfg config.HostCleanupConfig, targets func() []config.TargetConfig, logger *zap.Logger) *Manager {
	enabled := cfg.Enabled()
	for _, t := range targets() {
		if t.HostCleanup.Enabled() {
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

// Run cleans once at startup and then on each interval until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	if m == nil {
		return
	}
	m.Clean(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Clean(ctx)
		}
	}
}

// Clean applies one pass to every collection. Individual failures are logged
// and do not stop the other collections or the maintenance loop.
func (m *Manager) Clean(ctx context.Context) {
	if m == nil {
		return
	}
	for _, target := range m.targets() {
		delay := m.server.Delay
		if target.HostCleanup.Enabled() {
			delay = target.HostCleanup.Delay
		}
		if delay <= 0 {
			continue
		}
		removed, err := m.store.PruneHosts(ctx, target.Name, delay)
		if err != nil {
			m.logger.Warn("host cleanup failed",
				zap.String("collection", target.Name), zap.Error(err))
			continue
		}
		if removed > 0 {
			m.logger.Info("removed stale hosts",
				zap.String("collection", target.Name), zap.Int64("hosts", removed))
		}
	}
}
