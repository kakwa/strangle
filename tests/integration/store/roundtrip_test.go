package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kakwa/strangle/store"
	"github.com/kakwa/strangle/tests/testutil"
)

// openTestDB opens a private PostgreSQL database for one test. Tests skip
// unless STRANGLE_TEST_DSN is set.
func openTestDB(t *testing.T) (*store.DB, string) {
	t.Helper()
	dsn := testutil.TestDSN(t, "db")
	d, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}
	return d, dsn
}

func TestDBRoundTrip(t *testing.T) {
	d, _ := openTestDB(t)
	defer d.Close()
	ctx := context.Background()

	t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	in := store.RunInput{
		Run: store.RunRecord{
			Target: "home", Playbook: "site.yml",
			StartedAt: t1.Add(-30 * time.Second), FinishedAt: t1,
			DurationMS: 30000, ExitCode: 0, Success: true, Hosts: 2, ChangedHosts: 1,
			Output: "PLAY [all] ***\ndb output\n",
		},
		Hosts: []store.HostRunRecord{
			{Target: "home", Host: "h1", Status: "changed", Ok: 10, Changed: 2},
			{Target: "home", Host: "h2", Status: "ok", Ok: 10},
		},
		Tasks: []store.TaskStateRecord{
			{Host: "h1", Task: "Install nginx", Status: "changed", LastRunAt: t1, LastDiff: "-a\n+b"},
			{Host: "h1", Task: "Start nginx", Status: "ok", LastRunAt: t1},
		},
		Facts: map[string]json.RawMessage{"h1": []byte(`{"ansible_hostname":"h1"}`)},
	}
	if err := d.SaveRun(ctx, in); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}

	runs, err := d.ListRuns(ctx, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns() = %v, %v", runs, err)
	}
	if runs[0].ID == 0 {
		t.Fatal("run ID not assigned")
	}
	full, err := d.Run(ctx, runs[0].ID)
	if err != nil || full.Output != "PLAY [all] ***\ndb output\n" {
		t.Fatalf("Run() = %+v, %v; want the captured output", full, err)
	}
	if _, err := d.Run(ctx, 999999); err == nil {
		t.Error("Run(unknown) error = nil, want an error")
	}

	// Only hosts seen in the recap exist; h3 never ran, so it has no row.
	hosts, err := d.ListHosts(ctx)
	if err != nil || len(hosts) != 2 {
		t.Fatalf("ListHosts() = %v, %v", hosts, err)
	}
	byName := map[string]store.HostRecord{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	if h1 := byName["h1"]; h1.LastStatus != "changed" {
		t.Errorf("h1 = %+v", byName["h1"])
	}
	testutil.AssertJSONEqual(t, byName["h1"].Facts, []byte(`{"ansible_hostname":"h1"}`))
	if _, ok := byName["h3"]; ok {
		t.Error("h3 was never in the recap, want no host row")
	}

	hostRuns, err := d.HostRuns(ctx, "h1", 10)
	if err != nil || len(hostRuns) != 1 {
		t.Fatalf("HostRuns(h1) = %v, %v", hostRuns, err)
	}
	if hostRuns[0].RunID != runs[0].ID || hostRuns[0].Playbook != "site.yml" ||
		hostRuns[0].Status != "changed" || !hostRuns[0].StartedAt.Equal(t1.Add(-30*time.Second)) {
		t.Errorf("HostRuns(h1)[0] = %+v", hostRuns[0])
	}

	changes, err := d.HostChanges(ctx, "h1", 10)
	if err != nil || len(changes) != 2 {
		t.Fatalf("HostChanges(h1) = %v, %v", changes, err)
	}
	var install *store.TaskStateRecord
	for i := range changes {
		if changes[i].Task == "Install nginx" {
			install = &changes[i]
		}
	}
	if install == nil || install.LastDiff != "-a\n+b" || install.LastChangedAt == nil {
		t.Errorf("Install nginx change = %+v", install)
	}

	taskHistory, err := d.RunTasks(ctx, runs[0].ID)
	if err != nil || len(taskHistory) != 2 {
		t.Fatalf("RunTasks() = %v, %v; want both parsed task results", taskHistory, err)
	}
	if taskHistory[0].Host != "h1" || taskHistory[0].Task != "Install nginx" ||
		taskHistory[0].Status != "changed" || taskHistory[0].Diff != "-a\n+b" ||
		taskHistory[0].RunID != runs[0].ID {
		t.Errorf("RunTasks()[0] = %+v", taskHistory[0])
	}
	if _, err := d.RunTasks(ctx, 999999); err == nil {
		t.Error("RunTasks(unknown) error = nil, want an error")
	}

	// A second run must not duplicate host-run rows.
	in2 := store.RunInput{
		Run: store.RunRecord{
			Target: "home", Playbook: "site.yml",
			StartedAt: t2.Add(-10 * time.Second), FinishedAt: t2,
			DurationMS: 10000, ExitCode: 0, Success: true, Hosts: 1,
		},
		// h1 is absent from the recap, so its stored state is left as-is.
		Hosts: []store.HostRunRecord{{Target: "home", Host: "h2", Status: "ok", Ok: 5}},
	}
	if err := d.SaveRun(ctx, in2); err != nil {
		t.Fatalf("SaveRun() #2 error: %v", err)
	}

	// h1 was not recap'd in run 2, so its stored state stays at run 1.
	hosts, _ = d.ListHosts(ctx)
	byName = map[string]store.HostRecord{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	if h1 := byName["h1"]; !h1.LastRunAt.Equal(t1) || h1.LastStatus != "changed" {
		t.Errorf("h1 state should be untouched by run 2: %+v", h1)
	}

	// The same host name in another target is a distinct host.
	in3 := store.RunInput{
		Run: store.RunRecord{
			Target: "other", Playbook: "site.yml",
			StartedAt: t2.Add(-5 * time.Second), FinishedAt: t2,
			DurationMS: 5000, ExitCode: 0, Success: true, Hosts: 1,
		},
		Hosts: []store.HostRunRecord{{Target: "other", Host: "h1", Status: "ok", Ok: 3}},
	}
	if err := d.SaveRun(ctx, in3); err != nil {
		t.Fatalf("SaveRun() #3 error: %v", err)
	}

	hosts, _ = d.ListHosts(ctx)
	if len(hosts) != 3 {
		t.Fatalf("ListHosts() = %d hosts, want 3 (2 home + 1 other)", len(hosts))
	}
	byTarget := map[string]int{}
	for _, h := range hosts {
		byTarget[h.Target+"/"+h.Name]++
	}
	if byTarget["home/h1"] != 1 || byTarget["other/h1"] != 1 {
		t.Errorf("hosts by (target, name) = %v", byTarget)
	}

	hostRuns, err = d.HostRuns(ctx, "h1", 10)
	if err != nil || len(hostRuns) != 2 {
		t.Fatalf("HostRuns(h1) = %v, %v; want 1 per target", hostRuns, err)
	}
	targetsSeen := map[string]bool{}
	for _, hr := range hostRuns {
		targetsSeen[hr.Target] = true
	}
	if !targetsSeen["home"] || !targetsSeen["other"] {
		t.Errorf("HostRuns(h1) targets = %v, want home and other", targetsSeen)
	}

	// All three saved runs remain visible through ListRuns, including the one
	// in the other collection.
	runs, err = d.ListRuns(ctx, 10)
	if err != nil || len(runs) != 3 {
		t.Fatalf("ListRuns() = %v, %v; want 3 saved runs", runs, err)
	}
	byPlaybook := map[string]int{}
	for _, r := range runs {
		byPlaybook[r.Target+"/"+r.Playbook]++
	}
	if byPlaybook["home/site.yml"] != 2 || byPlaybook["other/site.yml"] != 1 {
		t.Errorf("runs by (target, playbook) = %v", byPlaybook)
	}
}

func TestDBTaskHistoryAcrossRuns(t *testing.T) {
	d, _ := openTestDB(t)
	defer d.Close()
	ctx := context.Background()
	base := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)

	for i, status := range []string{"changed", "ok"} {
		in := store.RunInput{
			Run: store.RunRecord{
				Target: "home", Playbook: "site.yml",
				StartedAt:  base.Add(time.Duration(i) * time.Hour),
				FinishedAt: base.Add(time.Duration(i)*time.Hour + time.Minute),
				Success:    true, Hosts: 1,
			},
			Hosts: []store.HostRunRecord{{Target: "home", Host: "h1", Status: status, Ok: 1}},
			Tasks: []store.TaskStateRecord{{
				Target: "home", Host: "h1", Task: "Install nginx", Status: status,
				LastDiff: "diff-" + status,
			}},
		}
		if err := d.SaveRun(ctx, in); err != nil {
			t.Fatalf("SaveRun() #%d error: %v", i+1, err)
		}
	}

	runs, err := d.ListRuns(ctx, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("ListRuns() = %v, %v", runs, err)
	}
	for _, run := range runs {
		history, err := d.RunTasks(ctx, run.ID)
		if err != nil || len(history) != 1 {
			t.Fatalf("RunTasks(%d) = %v, %v; want one row per run", run.ID, history, err)
		}
		if history[0].RunID != run.ID || history[0].Task != "Install nginx" || history[0].Diff == "" {
			t.Errorf("RunTasks(%d)[0] = %+v", run.ID, history[0])
		}
	}
	changes, err := d.HostChanges(ctx, "h1", 10)
	if err != nil || len(changes) != 1 {
		t.Fatalf("HostChanges(h1) = %v, %v; want one latest state", changes, err)
	}
}
