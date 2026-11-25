// Package notify sends notifications when a playbook run fails.
//
// Notifiers are configured per collection (a collection.yml target) and are
// resolved into validated config.NotificationConfig values at load time, so
// nothing in a git-backed repository can change where credentials are sent.
package notify

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
)

const maxOutput = 4000

// Event is the JSON payload sent to webhooks and the text source for Slack and
// email notifiers. It intentionally contains a bounded tail of the playbook
// output rather than the whole (potentially very large) log.
type Event struct {
	Event       string    `json:"event"`
	Org         string    `json:"org,omitempty"`
	Collection  string    `json:"collection"`
	Playbook    string    `json:"playbook"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	DurationMS  int64     `json:"duration_ms"`
	ExitCode    int       `json:"exit_code"`
	Hosts       int       `json:"hosts"`
	FailedHosts int       `json:"failed_hosts"`
	Output      string    `json:"output,omitempty"`
}

// FailureNotifier is the small interface the applier depends on. Tests can
// provide their own implementation; production uses Dispatcher.
type FailureNotifier interface {
	NotifyFailure(ctx context.Context, target config.TargetConfig, run store.RunRecord) error
}

// Dispatcher fans one failed run out to every notifier of its collection.
type Dispatcher struct {
	senders map[string]sender
	logger  *zap.Logger
}

type sender interface {
	Send(ctx context.Context, cfg config.NotificationConfig, event Event) error
}

// New returns a Dispatcher with the built-in webhook, Slack and email
// notifiers.
func New(logger *zap.Logger) *Dispatcher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Dispatcher{
		senders: map[string]sender{
			config.NotificationWebhook: webhookSender{},
			config.NotificationSlack:   slackSender{},
			config.NotificationEmail:   emailSender{},
		},
		logger: logger,
	}
}

// NotifyFailure sends one event to each configured notifier. Each send has
// its own timeout and runs in parallel, so a slow or broken endpoint cannot
// delay the others. Notification failures are returned to the caller (which
// logs them) but never change the outcome of the playbook run.
func (d *Dispatcher) NotifyFailure(ctx context.Context, target config.TargetConfig, run store.RunRecord) error {
	if len(target.Notifications) == 0 {
		return nil
	}
	event := Event{
		Event:       "run_failed",
		Org:         target.Org,
		Collection:  target.Name,
		Playbook:    run.Playbook,
		StartedAt:   run.StartedAt,
		FinishedAt:  run.FinishedAt,
		DurationMS:  run.DurationMS,
		ExitCode:    run.ExitCode,
		Hosts:       run.Hosts,
		FailedHosts: run.FailedHosts,
		Output:      tail(run.Output, maxOutput),
	}

	// The run's context may already be canceled during shutdown; notifications
	// are best-effort but should still be attempted.
	base := context.WithoutCancel(ctx)
	var wg sync.WaitGroup
	errs := make(chan error, len(target.Notifications))
	for _, cfg := range target.Notifications {
		s, ok := d.senders[cfg.Type]
		if !ok {
			errs <- fmt.Errorf("unknown notification type %q", cfg.Type)
			continue
		}
		wg.Add(1)
		go func(cfg config.NotificationConfig, s sender) {
			defer wg.Done()
			timeout := cfg.Timeout
			if timeout <= 0 {
				timeout = 10 * time.Second
			}
			sendCtx, cancel := context.WithTimeout(base, timeout)
			defer cancel()
			if err := s.Send(sendCtx, cfg, event); err != nil {
				errs <- fmt.Errorf("%s: %w", cfg.Type, err)
			}
		}(cfg, s)
	}
	wg.Wait()
	close(errs)

	var all []error
	for err := range errs {
		all = append(all, err)
	}
	if len(all) > 0 {
		d.logger.Warn("failure notifications failed",
			zap.String("collection", target.Name),
			zap.String("playbook", run.Playbook),
			zap.Error(errors.Join(all...)),
		)
	}
	return errors.Join(all...)
}

// tail keeps the end of a log, which is where Ansible reports its failures.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// summary is the one-line description reused by Slack and email notifiers.
func summary(event Event) string {
	return fmt.Sprintf("Strangle: %s/%s failed (exit code %d, %d/%d hosts failed, %dms)",
		event.Collection, event.Playbook, event.ExitCode, event.FailedHosts, event.Hosts, event.DurationMS)
}
