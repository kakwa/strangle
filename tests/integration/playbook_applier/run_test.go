package playbook_applier_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/kakwa/strangle/playbook_applier"
	"github.com/kakwa/strangle/tests/testutil"
	"slices"
	"strings"
	"testing"

	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/store"
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
	apr := playbook_applier.NewAnsible(
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
	if err := st.SaveRun(ctx, store.RunInput{Run: store.RunRecord{
		ID: id, Target: "prod", Playbook: "site", State: store.RunStateQueued,
	}}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	var stateDuringRun string
	apr := playbook_applier.NewAnsible(
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
	err := apr.Trigger(playbook_applier.WithRunID(ctx, id), "prod", "site")
	if err == nil || !strings.Contains(err.Error(), "worker socket closed") {
		t.Fatalf("Trigger() error = %v, want the runner error", err)
	}
	if stateDuringRun != store.RunStateRunning {
		t.Errorf("state during run = %q, want %q", stateDuringRun, store.RunStateRunning)
	}

	run, err := st.Run(ctx, id)
	if err != nil {
		t.Fatalf("Run(%d) error: %v", id, err)
	}
	runs, _ := st.ListRuns(ctx, 10)
	if len(runs) != 1 || runs[0].ID != id {
		t.Errorf("runs = %v, want the pre-created run ID to be reused", runs)
	}
	if run.State != store.RunStateCompleted || run.Success || run.ExitCode != 3 {
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
	if err := st.SaveRun(ctx, store.RunInput{Run: store.RunRecord{
		ID: id, Target: "prod", Playbook: "site", State: store.RunStateQueued,
	}}); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	runCtx, cancel := context.WithCancel(playbook_applier.WithRunID(ctx, id))
	apr := playbook_applier.NewAnsible(
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
	if !run.Canceled || run.State != store.RunStateCancelled || run.Success {
		t.Errorf("recorded run = %+v, want a cancelled, unsuccessful run", run)
	}
	if !strings.Contains(run.Output, "run canceled") {
		t.Errorf("output = %q, want the cancellation marker", run.Output)
	}
}

func TestHostsComeOnlyFromRecap(t *testing.T) {
	ctx := context.Background()
	st := testutil.OpenDB(t, "applier")
	apr := playbook_applier.NewAnsible(
		[]cfg.TargetConfig{namedTarget("prod", t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			return 0, "PLAY RECAP *******\n"+
				"web1 : ok=1    changed=0    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0\n"+
				"db1  : ok=1    changed=1    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0\n", nil, nil
		}),
		st, zap.NewNop(),
	)
	if err := apr.Trigger(ctx, "prod", "site"); err != nil {
		t.Fatalf("Trigger() error: %v", err)
	}

	// Only recap'd hosts exist; web2 never ran, so it has no row.
	hosts, err := st.ListHosts(ctx)
	if err != nil || len(hosts) != 2 {
		t.Fatalf("ListHosts() = %v, %v; want the two recap'd hosts", hosts, err)
	}
	byName := map[string]store.HostRecord{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	if web1 := byName["web1"]; web1.LastStatus != "ok" {
		t.Errorf("web1 = %+v, want status ok", web1)
	}
	if db1 := byName["db1"]; db1.LastStatus != "changed" {
		t.Errorf("db1 = %+v, want status changed", db1)
	}
	if _, ok := byName["web2"]; ok {
		t.Error("web2 was never in the recap, want no host row")
	}
}
