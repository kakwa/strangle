package db

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/store"
)

// openTestDB connects to the test database and clears all tables. Set
// STRANGLE_TEST_DSN to run these tests against a real PostgreSQL instance.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("STRANGLE_TEST_DSN")
	if dsn == "" {
		t.Skip("STRANGLE_TEST_DSN not set")
	}
	d, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	ctx := context.Background()
	for _, table := range []string{"task_states", "host_runs", "hosts", "runs"} {
		if err := d.db.WithContext(ctx).Exec("TRUNCATE " + table + " CASCADE").Error; err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	return d
}

func TestDBRoundTrip(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()
	ctx := context.Background()

	t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	in := store.RunInput{
		Run: store.RunRecord{
			Playbook: "site.yml", StartedAt: t1.Add(-30 * time.Second), FinishedAt: t1,
			DurationMS: 30000, ExitCode: 0, Success: true, Hosts: 2, ChangedHosts: 1,
		},
		Hosts: []store.HostRunRecord{
			{Host: "h1", Status: "changed", Ok: 10, Changed: 2},
			{Host: "h2", Status: "ok", Ok: 10},
		},
		Tasks: []store.TaskStateRecord{
			{Host: "h1", Task: "Install nginx", Status: "changed", LastRunAt: t1, LastDiff: "-a\n+b"},
			{Host: "h1", Task: "Start nginx", Status: "ok", LastRunAt: t1},
		},
		ScopeNames: []string{"h1", "h2", "h3"},
		Inventory:  []store.InventoryHost{{Name: "h1", IP: "10.0.0.1", Groups: []string{"web"}}, {Name: "h2"}, {Name: "h3"}},
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

	hosts, err := d.ListHosts(ctx)
	if err != nil || len(hosts) != 3 {
		t.Fatalf("ListHosts() = %v, %v", hosts, err)
	}
	byName := map[string]store.HostRecord{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	if !byName["h1"].Active || !byName["h1"].InInventory || byName["h1"].IP != "10.0.0.1" {
		t.Errorf("h1 = %+v", byName["h1"])
	}
	if got := byName["h1"].Groups; len(got) != 1 || got[0] != "web" {
		t.Errorf("h1.Groups = %v", byName["h1"].Groups)
	}
	if byName["h3"].Active {
		t.Error("h3 in scope but not in recap, want inactive")
	}
	if byName["h3"].DisappearedAt == nil || !byName["h3"].DisappearedAt.Equal(t1) {
		t.Errorf("h3.DisappearedAt = %v, want %v", byName["h3"].DisappearedAt, t1)
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

	// A second run must not duplicate host-run rows.
	in2 := store.RunInput{
		Run: store.RunRecord{
			Playbook: "site.yml", StartedAt: t2.Add(-10 * time.Second), FinishedAt: t2,
			DurationMS: 10000, ExitCode: 0, Success: true, Hosts: 1,
		},
		Hosts: []store.HostRunRecord{{Host: "h2", Status: "ok", Ok: 5}},
		// h1 is in scope but absent from the recap, so it becomes disappeared.
		ScopeNames: []string{"h1", "h2"},
		Inventory: []store.InventoryHost{
			{Name: "h1"}, {Name: "h2"}, // h3 dropped from the inventory
		},
	}
	if err := d.SaveRun(ctx, in2); err != nil {
		t.Fatalf("SaveRun() #2 error: %v", err)
	}

	hosts, _ = d.ListHosts(ctx)
	byName = map[string]store.HostRecord{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	if byName["h3"].InInventory {
		t.Error("h3 dropped from inventory, want in_inventory=false")
	}
	if byName["h3"].Active || byName["h3"].DisappearedAt == nil {
		t.Errorf("h3 should keep its disappeared state: %+v", byName["h3"])
	}

	stats, err := d.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error: %v", err)
	}
	if stats.Hosts.Total != 3 || stats.Hosts.InInventory != 2 || stats.Hosts.Active != 1 || stats.Hosts.Disappeared != 1 {
		t.Errorf("Stats().Hosts = %+v", stats.Hosts)
	}
	if stats.Runs.Total != 2 {
		t.Errorf("Stats().Runs = %+v", stats.Runs)
	}
	if len(stats.PerPlaybook) != 1 || stats.PerPlaybook[0].Total != 2 {
		t.Errorf("Stats().PerPlaybook = %+v", stats.PerPlaybook)
	}
}
