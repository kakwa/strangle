package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/internal/testutil"
)

func openStore(t *testing.T) *db.DB {
	t.Helper()
	return testutil.OpenDB(t, "store")
}

func testRun(target, playbook string, finished time.Time) store.RunRecord {
	started := finished.Add(-time.Duration(len(playbook)) * time.Second)
	return store.RunRecord{
		Target:       target,
		Playbook:     playbook,
		StartedAt:    started,
		FinishedAt:   finished,
		DurationMS:   int64(len(playbook) * 1000),
		ExitCode:     0,
		Success:      true,
		Hosts:        2,
		ChangedHosts: 1,
		FailedHosts:  0,
	}
}

func TestStoreSaveRunAndQueries(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	defer s.Close()

	t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	in := store.RunInput{
		Run: func() store.RunRecord {
			r := testRun("home", "site.yml", t1)
			r.Output = "PLAY [all] ***\nfull output\n"
			return r
		}(),
		Hosts: []store.HostRunRecord{
			{Target: "home", Host: "h1", Status: "changed", Ok: 10, Changed: 2},
			{Target: "home", Host: "h2", Status: "ok", Ok: 10},
		},
		Tasks: []store.TaskStateRecord{
			{Target: "home", Host: "h1", Task: "Install nginx", Status: "changed", LastDiff: "-a\n+b"},
			{Target: "home", Host: "h1", Task: "Start nginx", Status: "ok"},
		},
		ScopeNames: []string{"h1", "h2", "h3"},
		Inventory:  []store.InventoryHost{{Name: "h1"}, {Name: "h2"}, {Name: "h3"}},
	}
	if err := s.SaveRun(ctx, in); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}

	runs, err := s.ListRuns(ctx, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns() = %v, %v", runs, err)
	}
	if runs[0].ID == 0 {
		t.Error("run ID not assigned")
	}
	full, err := s.Run(ctx, runs[0].ID)
	if err != nil || full.Output != "PLAY [all] ***\nfull output\n" {
		t.Errorf("Run() = %+v, %v; want the captured output", full, err)
	}
	if _, err := s.Run(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Run(unknown) error = %v, want store.ErrNotFound", err)
	}

	hosts, err := s.ListHosts(ctx)
	if err != nil || len(hosts) != 3 {
		t.Fatalf("ListHosts() = %v, %v", hosts, err)
	}
	byName := map[string]store.HostRecord{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	if !byName["h1"].Active || !byName["h1"].InInventory {
		t.Errorf("h1 = %+v, want active in-inventory", byName["h1"])
	}
	if byName["h1"].LastChangeAt == nil || !byName["h1"].LastChangeAt.Equal(t1) {
		t.Errorf("h1.LastChangeAt = %v, want %v", byName["h1"].LastChangeAt, t1)
	}
	if byName["h3"].Active {
		t.Error("h3 was in scope but not in the recap, want inactive")
	}
	if byName["h3"].DisappearedAt == nil || !byName["h3"].DisappearedAt.Equal(t1) {
		t.Errorf("h3.DisappearedAt = %v, want %v", byName["h3"].DisappearedAt, t1)
	}

	hostRuns, err := s.HostRuns(ctx, "h1", 10)
	if err != nil || len(hostRuns) != 1 {
		t.Fatalf("HostRuns(h1) = %v, %v", hostRuns, err)
	}
	if hostRuns[0].Status != "changed" || hostRuns[0].Changed != 2 {
		t.Errorf("HostRuns(h1)[0] = %+v", hostRuns[0])
	}
	if hostRuns[0].RunID != runs[0].ID || hostRuns[0].Playbook != "site.yml" {
		t.Errorf("HostRuns(h1)[0] run fields = %+v", hostRuns[0])
	}

	changes, err := s.HostChanges(ctx, "h1", 10)
	if err != nil || len(changes) != 2 {
		t.Fatalf("HostChanges(h1) = %v, %v", changes, err)
	}
	if changes[0].Task != "Install nginx" {
		t.Errorf("most recently changed first, got %q", changes[0].Task)
	}
	if changes[0].LastDiff != "-a\n+b" {
		t.Errorf("changes[0].LastDiff = %q", changes[0].LastDiff)
	}

	history, err := s.RunTasks(ctx, runs[0].ID)
	if err != nil || len(history) != 2 {
		t.Fatalf("RunTasks() = %v, %v; want both parsed task results", history, err)
	}
	if history[0].Task != "Install nginx" || history[0].Status != "changed" ||
		history[0].Diff != "-a\n+b" || history[0].RunID != runs[0].ID {
		t.Errorf("RunTasks()[0] = %+v", history[0])
	}
	if _, err := s.RunTasks(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("RunTasks(unknown) error = %v, want store.ErrNotFound", err)
	}

	// A host sharing a prefix must not leak its task states.
	in2 := store.RunInput{
		Run:   testRun("home", "site.yml", t2),
		Hosts: []store.HostRunRecord{{Target: "home", Host: "h10", Status: "ok", Ok: 1}},
		Tasks: []store.TaskStateRecord{{Target: "home", Host: "h10", Task: "Install nginx", Status: "changed", LastDiff: "x"}},
	}
	if err := s.SaveRun(ctx, in2); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
	run2, err := s.ListRuns(ctx, 1)
	if err != nil || len(run2) != 1 {
		t.Fatalf("ListRuns() after second save = %v, %v", run2, err)
	}
	history2, err := s.RunTasks(ctx, run2[0].ID)
	if err != nil || len(history2) != 1 || history2[0].Host != "h10" {
		t.Errorf("RunTasks(run 2) = %v, %v; want the h10 task", history2, err)
	}
	changes, err = s.HostChanges(ctx, "h1", 10)
	if err != nil || len(changes) != 2 {
		t.Fatalf("HostChanges(h1) after h10 run = %v, %v", changes, err)
	}
	for _, c := range changes {
		if c.Host != "h1" {
			t.Errorf("HostChanges(h1) leaked host %q", c.Host)
		}
	}

	// The same host name in another target is a distinct host: host runs and
	// task states stay scoped per target.
	in3 := store.RunInput{
		Run:   testRun("other", "site.yml", t2.Add(time.Second)),
		Hosts: []store.HostRunRecord{{Target: "other", Host: "h1", Status: "ok", Ok: 3}},
		Tasks: []store.TaskStateRecord{{Target: "other", Host: "h1", Task: "Reboot", Status: "changed", LastDiff: "y"}},
	}
	if err := s.SaveRun(ctx, in3); err != nil {
		t.Fatalf("SaveRun() #3 error: %v", err)
	}
	if hosts, _ := s.ListHosts(ctx); len(hosts) != 5 {
		t.Errorf("ListHosts() = %d hosts, want 5 (4 home + 1 other)", len(hosts))
	}
	hostRuns, err = s.HostRuns(ctx, "h1", 10)
	if err != nil || len(hostRuns) != 2 {
		t.Fatalf("HostRuns(h1) = %v, %v; want one per target", hostRuns, err)
	}
	targetsSeen := map[string]bool{}
	for _, hr := range hostRuns {
		targetsSeen[hr.Target] = true
	}
	if !targetsSeen["home"] || !targetsSeen["other"] {
		t.Errorf("HostRuns(h1) targets = %v, want home and other", targetsSeen)
	}
	changes, err = s.HostChanges(ctx, "h1", 10)
	if err != nil || len(changes) != 3 {
		t.Fatalf("HostChanges(h1) = %v, %v; want 2 home + 1 other", changes, err)
	}

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("store.Stats() error: %v", err)
	}
	if stats.Hosts.Total != 5 || stats.Hosts.InInventory != 3 || stats.Hosts.Active != 4 || stats.Hosts.Disappeared != 1 {
		t.Errorf("store.Stats().Hosts = %+v", stats.Hosts)
	}
	if stats.Runs.Total != 3 || stats.Runs.Succeeded != 3 {
		t.Errorf("store.Stats().Runs = %+v", stats.Runs)
	}
	perPB := map[string]int{}
	for _, pb := range stats.PerPlaybook {
		perPB[pb.Target+"/"+pb.Playbook] = pb.Total
	}
	if perPB["home/site.yml"] != 2 || perPB["other/site.yml"] != 1 {
		t.Errorf("store.Stats().PerPlaybook = %v", perPB)
	}
}

func TestStoreFactsAreRefreshedOnlyWhenPresent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	defer s.Close()

	facts := json.RawMessage(`{"ansible_hostname":"web1"}`)
	in := store.RunInput{
		Run:   testRun("home", "site.yml", time.Now().UTC()),
		Hosts: []store.HostRunRecord{{Host: "web1", Status: "ok", Ok: 1}},
		Facts: map[string]json.RawMessage{"web1": facts},
	}
	if err := s.SaveRun(ctx, in); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
	host := func() store.HostRecord {
		t.Helper()
		hosts, err := s.ListHosts(ctx)
		if err != nil || len(hosts) != 1 {
			t.Fatalf("ListHosts() = %v, %v", hosts, err)
		}
		return hosts[0]
	}
	testutil.AssertJSONEqual(t, host().Facts, facts)

	in.Run = testRun("home", "site.yml", time.Now().UTC().Add(time.Hour))
	in.Facts = nil
	if err := s.SaveRun(ctx, in); err != nil {
		t.Fatalf("SaveRun() without facts: %v", err)
	}
	if got := string(host().Facts); got != string(facts) {
		t.Errorf("Facts after run without facts = %s, want %s", got, facts)
	}

	updated := json.RawMessage(`{"ansible_hostname":"web1","os":"linux"}`)
	in.Run = testRun("home", "site.yml", time.Now().UTC().Add(2*time.Hour))
	in.Facts = map[string]json.RawMessage{"web1": updated}
	if err := s.SaveRun(ctx, in); err != nil {
		t.Fatalf("SaveRun() with updated facts: %v", err)
	}
	if got := string(host().Facts); got != string(updated) {
		t.Errorf("Facts after refresh = %s, want %s", got, updated)
	}
}

func TestStoreEmptyCollectionsAreNotNull(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	defer s.Close()

	if runs, err := s.HostRuns(ctx, "missing", 10); err != nil || runs == nil {
		t.Fatalf("HostRuns() = %v, %v; want non-nil empty slice", runs, err)
	}
	if changes, err := s.HostChanges(ctx, "missing", 10); err != nil || changes == nil {
		t.Fatalf("HostChanges() = %v, %v; want non-nil empty slice", changes, err)
	}
	if stats, err := s.Stats(ctx); err != nil || stats.PerPlaybook == nil {
		t.Fatalf("store.Stats().PerPlaybook = %v, %v; want non-nil empty slice", stats, err)
	}
}
