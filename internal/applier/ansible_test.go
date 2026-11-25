package applier

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
)

type executorFunc func(ctx context.Context, collection, scope, workingDir string, args ...string) (int, string, error)

func (f executorFunc) RunPlaybook(ctx context.Context, collection, scope, workingDir string, args ...string) (int, string, error) {
	return f(ctx, collection, scope, workingDir, args...)
}

type recordingNotifier struct {
	runs   []store.RunRecord
	target config.TargetConfig
}

func (n *recordingNotifier) NotifyFailure(_ context.Context, target config.TargetConfig, run store.RunRecord) error {
	n.target = target
	n.runs = append(n.runs, run)
	return nil
}

func testTarget(dir string) config.TargetConfig {
	return config.TargetConfig{
		Name: "prod",
		Org:  "lab",
		Notifications: []config.NotificationConfig{{
			Type:    config.NotificationWebhook,
			URL:     "https://example.com/hook",
			Timeout: time.Second,
		}},
		WorkingDir: dir,
		Playbooks: []config.PlaybookConfig{{
			Name:       "site",
			TargetName: "prod",
			Playbook:   "site.yml",
			Inventory:  "inventory",
			WorkingDir: dir,
		}},
	}
}

func TestFailureNotifiesCollection(t *testing.T) {
	notifier := &recordingNotifier{}
	apr := NewAnsible(
		[]config.TargetConfig{testTarget(t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, error) {
			return 2, "fatal: boom", nil
		}),
		store.NewMem(), zap.NewNop(), notifier,
	)
	if err := apr.Trigger(context.Background(), "prod", "site"); err == nil {
		t.Fatal("Trigger() error = nil, want the failed playbook's error")
	}
	if len(notifier.runs) != 1 {
		t.Fatalf("notified %d times, want 1", len(notifier.runs))
	}
	if notifier.target.Name != "prod" || notifier.target.Org != "lab" ||
		len(notifier.target.Notifications) != 1 {
		t.Errorf("notified target = %+v", notifier.target)
	}
	run := notifier.runs[0]
	if run.ExitCode != 2 || run.Success || !strings.Contains(run.Output, "fatal: boom") {
		t.Errorf("notified run = %+v", run)
	}
}

func TestSuccessDoesNotNotify(t *testing.T) {
	notifier := &recordingNotifier{}
	apr := NewAnsible(
		[]config.TargetConfig{testTarget(t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, error) {
			return 0, "ok", nil
		}),
		store.NewMem(), zap.NewNop(), notifier,
	)
	if err := apr.Trigger(context.Background(), "prod", "site"); err != nil {
		t.Fatalf("Trigger() error: %v", err)
	}
	if len(notifier.runs) != 0 {
		t.Errorf("notified on success: %+v", notifier.runs)
	}
}
