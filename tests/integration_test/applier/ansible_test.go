package applier_test

import (
	"context"
	"encoding/json"
	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/tests/testutil"
	"slices"
	"strings"
	"testing"

	"github.com/kakwa/strangle/cfg"
	"go.uber.org/zap"
)

type executorFunc func(ctx context.Context, collection, scope, workingDir string, args ...string) (int, string, map[string]json.RawMessage, error)

func (f executorFunc) RunPlaybook(ctx context.Context, collection, scope, workingDir string, args ...string) (int, string, map[string]json.RawMessage, error) {
	return f(ctx, collection, scope, workingDir, args...)
}

func testTarget(dir string) cfg.TargetConfig {
	return cfg.TargetConfig{
		Name:       "prod",
		Org:        "lab",
		WorkingDir: dir,
		Playbooks: []cfg.PlaybookConfig{{
			Name:       "site",
			TargetName: "prod",
			Playbook:   "site.yml",
			Inventory:  "inventory",
			WorkingDir: dir,
		}},
	}
}

func TestCheckModeRun(t *testing.T) {
	st := testutil.OpenDB(t, "applier")
	var gotArgs []string
	apr := applier.NewAnsible(
		[]cfg.TargetConfig{testTarget(t.TempDir())},
		executorFunc(func(_ context.Context, _, _, _ string, args ...string) (int, string, map[string]json.RawMessage, error) {
			gotArgs = args
			return 0, "PLAY [all] ***\n", nil, nil
		}),
		st, zap.NewNop(),
	)
	if err := apr.Trigger(applier.WithCheck(context.Background(), true), "prod", "site"); err != nil {
		t.Fatalf("Trigger() error: %v", err)
	}
	if !slices.Contains(gotArgs, "--check") {
		t.Fatalf("executor args = %v, want --check", gotArgs)
	}
	runs, err := st.ListRuns(context.Background(), 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns() = %v, %v", runs, err)
	}
	if !runs[0].Check {
		t.Fatal("recorded run is not marked as a check run")
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
	st := testutil.OpenDB(t, "applier")
	apr := applier.NewAnsible(
		[]cfg.TargetConfig{testTarget(t.TempDir())},
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
	st := testutil.OpenDB(t, "applier")
	facts := map[string]json.RawMessage{"web1": []byte(`{"ansible_hostname":"web1"}`)}
	apr := applier.NewAnsible(
		[]cfg.TargetConfig{testTarget(t.TempDir())},
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
	testutil.AssertJSONEqual(t, hostFact(), facts["web1"])

	// A later run without facts must not erase the last known blob.
	apr = applier.NewAnsible(
		[]cfg.TargetConfig{testTarget(t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			return 0, output, nil, nil
		}),
		st, zap.NewNop(),
	)
	if err := apr.Trigger(context.Background(), "prod", "site"); err != nil {
		t.Fatalf("Trigger() error: %v", err)
	}
	testutil.AssertJSONEqual(t, hostFact(), facts["web1"])
}
