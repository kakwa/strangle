package applier_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/tests/testutil"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/utils/cfg"
	"go.uber.org/zap"
)

func namedTarget(name string, dir string) cfg.TargetConfig {
	return cfg.TargetConfig{
		Name: name,
		Playbooks: []cfg.PlaybookConfig{{
			Name:       "site",
			TargetName: name,
			Playbook:   "site.yml",
			Inventory:  "inventory",
			WorkingDir: dir,
		}},
	}
}

func TestRunCycleRecordsEveryTarget(t *testing.T) {
	ctx := context.Background()
	st := testutil.OpenDB(t, "applier")
	executed := make(chan string, 4)
	apr := applier.NewAnsible(
		[]cfg.TargetConfig{namedTarget("zeta", t.TempDir()), namedTarget("alpha", t.TempDir())},
		executorFunc(func(_ context.Context, target, _ string, _ string, _ ...string) (int, string, map[string]json.RawMessage, error) {
			executed <- target
			return 0, "PLAY RECAP *******\nweb1 : ok=1    changed=0    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0\n", nil, nil
		}),
		st, zap.NewNop(),
	)

	if err := apr.RunCycle(ctx); err != nil {
		t.Fatalf("RunCycle() error: %v", err)
	}
	if got := []string{<-executed, <-executed}; !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Errorf("execution order = %v, want sorted target names", got)
	}
	runs, err := st.ListRuns(ctx, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("ListRuns() = %v, %v; want two runs", runs, err)
	}
	targets := []string{runs[0].Target, runs[1].Target}
	if !slices.Contains(targets, "alpha") || !slices.Contains(targets, "zeta") {
		t.Errorf("recorded targets = %v", targets)
	}
}

func TestInFlightRunStateAndRunnerError(t *testing.T) {
	ctx := context.Background()
	st := testutil.OpenDB(t, "applier")
	id := uint64(42)
	if err := st.SaveRun(ctx, model.RunInput{Run: model.RunRecord{
		ID: id, Target: "prod", Playbook: "site", State: model.RunStateQueued,
	}}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	var stateDuringRun string
	apr := applier.NewAnsible(
		[]cfg.TargetConfig{namedTarget("prod", t.TempDir())},
		executorFunc(func(ctx context.Context, _, _, _ string, _ ...string) (int, string, map[string]json.RawMessage, error) {
			record, err := st.Run(ctx, id)
			if err != nil {
				t.Fatalf("read in-flight run: %v", err)
			}
			stateDuringRun = record.State
			return 3, "TASK [boom] *****\nfatal: boom", nil, errors.New("worker socket closed")
		}),
		st, zap.NewNop(),
	)
	err := apr.Trigger(applier.WithRunID(ctx, id), "prod", "site")
	if err == nil || !strings.Contains(err.Error(), "worker socket closed") {
		t.Fatalf("Trigger() error = %v, want the runner error", err)
	}
	if stateDuringRun != model.RunStateRunning {
		t.Errorf("state during run = %q, want %q", stateDuringRun, model.RunStateRunning)
	}

	run, err := st.Run(ctx, id)
	if err != nil {
		t.Fatalf("Run(%d) error: %v", id, err)
	}
	runs, _ := st.ListRuns(ctx, 10)
	if len(runs) != 1 || runs[0].ID != id {
		t.Errorf("runs = %v, want the pre-created run ID to be reused", runs)
	}
	if run.State != model.RunStateCompleted || run.Success || run.ExitCode != 3 {
		t.Errorf("recorded run = %+v, want a completed failed run", run)
	}
	if !strings.Contains(run.Output, "fatal: boom") ||
		!strings.Contains(run.Output, "runner error: worker socket closed") {
		t.Errorf("output = %q, want both the playbook and runner errors", run.Output)
	}
}

func TestCancellationIsRecorded(t *testing.T) {
	ctx := context.Background()
	st := testutil.OpenDB(t, "applier")
	id := uint64(7)
	if err := st.SaveRun(ctx, model.RunInput{Run: model.RunRecord{
		ID: id, Target: "prod", Playbook: "site", State: model.RunStateQueued,
	}}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	runCtx, cancel := context.WithCancel(applier.WithRunID(ctx, id))
	apr := applier.NewAnsible(
		[]cfg.TargetConfig{namedTarget("prod", t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			cancel()
			return 0, "", nil, nil
		}),
		st, zap.NewNop(),
	)
	if err := apr.Trigger(runCtx, "prod", "site"); err != nil {
		t.Fatalf("Trigger() error: %v", err)
	}
	run, err := st.Run(ctx, id)
	if err != nil {
		t.Fatalf("Run(%d) error: %v", id, err)
	}
	if !run.Canceled || run.State != model.RunStateCancelled || run.Success {
		t.Errorf("recorded run = %+v, want a cancelled, unsuccessful run", run)
	}
	if !strings.Contains(run.Output, "run canceled") {
		t.Errorf("output = %q, want the cancellation marker", run.Output)
	}
}

func TestInventoryScopeMarksMissingHostsDisappeared(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "inventory"), 0o755); err != nil {
		t.Fatal(err)
	}
	inventory := `all:
  children:
    web:
      hosts:
        web1:
          ansible_host: 10.0.0.1
        web2: {}
    db:
      hosts:
        db1: {}
`
	if err := os.WriteFile(filepath.Join(dir, "inventory", "hosts.yml"), []byte(inventory), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "site.yml"), []byte("- hosts: web\n  tasks: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := testutil.OpenDB(t, "applier")
	apr := applier.NewAnsible(
		[]cfg.TargetConfig{namedTarget("prod", dir)},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			return 0, "PLAY RECAP *******\nweb1 : ok=1    changed=0    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0\n", nil, nil
		}),
		st, zap.NewNop(),
	)
	if err := apr.Trigger(ctx, "prod", "site"); err != nil {
		t.Fatalf("Trigger() error: %v", err)
	}

	hosts, err := st.ListHosts(ctx)
	if err != nil || len(hosts) != 3 {
		t.Fatalf("ListHosts() = %v, %v; want all three inventory hosts", hosts, err)
	}
	byName := map[string]model.HostRecord{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	if web1 := byName["web1"]; !web1.Active || web1.DisappearedAt != nil || web1.IP != "10.0.0.1" {
		t.Errorf("web1 = %+v, want an active host with its inventory IP", web1)
	}
	if web2 := byName["web2"]; web2.Active || web2.DisappearedAt == nil {
		t.Errorf("web2 = %+v, want a disappeared host (in scope but absent from the recap)", web2)
	}
	if db1 := byName["db1"]; !db1.InInventory || db1.Active || db1.DisappearedAt != nil {
		t.Errorf("db1 = %+v, want an inventory host outside this play's scope", db1)
	}
}
