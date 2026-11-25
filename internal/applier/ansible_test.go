package applier

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
)

type executorFunc func(ctx context.Context, collection, scope, workingDir string, args ...string) (int, string, map[string]json.RawMessage, error)

func (f executorFunc) RunPlaybook(ctx context.Context, collection, scope, workingDir string, args ...string) (int, string, map[string]json.RawMessage, error) {
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
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			return 2, "fatal: boom", nil, nil
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

func TestTaskResultsAreKeptForEveryRun(t *testing.T) {
	const output = `TASK [Install nginx] *******
--- before
+++ after
@@ -1 +1 @@
-old
+new
changed: [web1]

PLAY RECAP *******
web1 : ok=1    changed=1    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0
`
	st := store.NewMem()
	apr := NewAnsible(
		[]config.TargetConfig{testTarget(t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			return 0, output, nil, nil
		}),
		st, zap.NewNop(),
	)
	for range 2 {
		if err := apr.Trigger(context.Background(), "prod", "site"); err != nil {
			t.Fatalf("Trigger() error: %v", err)
		}
	}

	runs, err := st.ListRuns(context.Background(), 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("ListRuns() = %v, %v", runs, err)
	}
	for _, run := range runs {
		tasks, err := st.RunTasks(context.Background(), run.ID)
		if err != nil || len(tasks) != 1 {
			t.Fatalf("RunTasks(%d) = %v, %v; want one task result", run.ID, tasks, err)
		}
		if tasks[0].Task != "Install nginx" || tasks[0].Host != "web1" ||
			tasks[0].Status != "changed" || !strings.Contains(tasks[0].Diff, "+new") ||
			tasks[0].RunID != run.ID {
			t.Errorf("RunTasks(%d)[0] = %+v", run.ID, tasks[0])
		}
	}
}

func TestFactsAreSavedAndRefreshed(t *testing.T) {
	const output = `TASK [Gathering Facts] *******
ok: [web1]

PLAY RECAP *******
web1 : ok=1    changed=0    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0
`
	st := store.NewMem()
	facts := map[string]json.RawMessage{"web1": []byte(`{"ansible_hostname":"web1"}`)}
	apr := NewAnsible(
		[]config.TargetConfig{testTarget(t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			return 0, output, facts, nil
		}),
		st, zap.NewNop(),
	)
	if err := apr.Trigger(context.Background(), "prod", "site"); err != nil {
		t.Fatalf("Trigger() error: %v", err)
	}

	hostFact := func() json.RawMessage {
		t.Helper()
		hosts, err := st.ListHosts(context.Background())
		if err != nil || len(hosts) != 1 {
			t.Fatalf("ListHosts() = %v, %v", hosts, err)
		}
		return hosts[0].Facts
	}
	if got := string(hostFact()); got != string(facts["web1"]) {
		t.Errorf("Facts = %s, want %s", got, facts["web1"])
	}

	// A later run without facts must not erase the last known blob.
	apr = NewAnsible(
		[]config.TargetConfig{testTarget(t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			return 0, output, nil, nil
		}),
		st, zap.NewNop(),
	)
	if err := apr.Trigger(context.Background(), "prod", "site"); err != nil {
		t.Fatalf("Trigger() error: %v", err)
	}
	if got := string(hostFact()); got != string(facts["web1"]) {
		t.Errorf("Facts after run without facts = %s, want %s", got, facts["web1"])
	}
}

func TestSuccessDoesNotNotify(t *testing.T) {
	notifier := &recordingNotifier{}
	apr := NewAnsible(
		[]config.TargetConfig{testTarget(t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			return 0, "ok", nil, nil
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
