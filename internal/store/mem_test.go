package store

import (
	"context"
	"testing"
	"time"
)

func testRun(target, playbook string, finished time.Time) RunRecord {
	started := finished.Add(-time.Duration(len(playbook)) * time.Second)
	return RunRecord{
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

func TestMemSaveRunAndQueries(t *testing.T) {
	ctx := context.Background()
	s := NewMem()
	defer s.Close()

	t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	in := RunInput{
		Run: testRun("home", "site.yml", t1),
		Hosts: []HostRunRecord{
			{Target: "home", Host: "h1", Status: "changed", Ok: 10, Changed: 2},
			{Target: "home", Host: "h2", Status: "ok", Ok: 10},
		},
		Tasks: []TaskStateRecord{
			{Target: "home", Host: "h1", Task: "Install nginx", Status: "changed", LastDiff: "-a\n+b"},
			{Target: "home", Host: "h1", Task: "Start nginx", Status: "ok"},
		},
		ScopeNames: []string{"h1", "h2", "h3"},
		Inventory:  []InventoryHost{{Name: "h1"}, {Name: "h2"}, {Name: "h3"}},
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

	hosts, err := s.ListHosts(ctx)
	if err != nil || len(hosts) != 3 {
		t.Fatalf("ListHosts() = %v, %v", hosts, err)
	}
	byName := map[string]HostRecord{}
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

	// A host sharing a prefix must not leak its task states.
	in2 := RunInput{
		Run:   testRun("home", "site.yml", t2),
		Hosts: []HostRunRecord{{Target: "home", Host: "h10", Status: "ok", Ok: 1}},
		Tasks: []TaskStateRecord{{Target: "home", Host: "h10", Task: "Install nginx", Status: "changed", LastDiff: "x"}},
	}
	if err := s.SaveRun(ctx, in2); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
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
	in3 := RunInput{
		Run:   testRun("other", "site.yml", t2.Add(time.Second)),
		Hosts: []HostRunRecord{{Target: "other", Host: "h1", Status: "ok", Ok: 3}},
		Tasks: []TaskStateRecord{{Target: "other", Host: "h1", Task: "Reboot", Status: "changed", LastDiff: "y"}},
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
		t.Fatalf("Stats() error: %v", err)
	}
	if stats.Hosts.Total != 5 || stats.Hosts.InInventory != 3 || stats.Hosts.Active != 4 || stats.Hosts.Disappeared != 1 {
		t.Errorf("Stats().Hosts = %+v", stats.Hosts)
	}
	if stats.Runs.Total != 3 || stats.Runs.Succeeded != 3 {
		t.Errorf("Stats().Runs = %+v", stats.Runs)
	}
	perPB := map[string]int{}
	for _, pb := range stats.PerPlaybook {
		perPB[pb.Target+"/"+pb.Playbook] = pb.Total
	}
	if perPB["home/site.yml"] != 2 || perPB["other/site.yml"] != 1 {
		t.Errorf("Stats().PerPlaybook = %v", perPB)
	}
}
