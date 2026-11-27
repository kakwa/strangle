package model

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestDialectorFor(t *testing.T) {
	cases := []struct {
		dsn  string
		name string
		err  string // expected error substring; empty means success
	}{
		{"postgres://user:pass@host:5432/db", "postgres", ""},
		{"postgresql://user@host/db", "postgres", ""},
		{"POSTGRES://host/db", "postgres", ""},
		{"/var/lib/strangle/strangle.db", "sqlite", ""},
		{"./data.db", "sqlite", ""},
		{"file:/var/lib/strangle/strangle.db", "sqlite", ""},
		{"sqlite:///var/lib/strangle/strangle.db", "sqlite", ""},
		{"sqlite3://./data.db", "sqlite", ""},
		{"", "", "empty"},
		{"   ", "", "empty"},
	}
	for _, c := range cases {
		d, err := dialectorFor(c.dsn)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("dialectorFor(%q) = (%q, %v), want error containing %q", c.dsn, d.name, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("dialectorFor(%q) error: %v", c.dsn, err)
		}
		if d.name != c.name {
			t.Fatalf("dialectorFor(%q).name = %q, want %q", c.dsn, d.name, c.name)
		}
		if d.open == nil {
			t.Fatalf("dialectorFor(%q) returned a nil dialector", c.dsn)
		}
	}
}

// TestSQLiteRoundTrip exercises the Store surface on a temporary SQLite file,
// the backend a bare file DSN selects. It covers the queries rewritten to be
// dialect-portable (ListHosts, HostRuns), jsonb-as-BLOB columns, the host
// inventory-maintenance paths, and unique-constraint translation to
// gorm.ErrDuplicatedKey.
func TestSQLiteRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "strangle.db")
	d, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	defer d.Close()

	ctx := context.Background()
	t1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	t3 := t2.Add(time.Hour)

	// Unique-constraint translation must hold on SQLite as on PostgreSQL.
	if _, err := d.CreateOrg(ctx, "acme"); err != nil {
		t.Fatalf("CreateOrg(acme): %v", err)
	}
	if _, err := d.CreateOrg(ctx, "acme"); !errors.Is(err, gorm.ErrDuplicatedKey) {
		t.Fatalf("CreateOrg(acme) again: want gorm.ErrDuplicatedKey, got %v", err)
	}
	orgs, err := d.ListOrgs(ctx)
	if err != nil {
		t.Fatalf("ListOrgs: %v", err)
	}
	if len(orgs) != 2 {
		t.Fatalf("ListOrgs: want default + acme, got %d orgs", len(orgs))
	}

	save := func(rec RunRecord, hosts []HostRunRecord, tasks []TaskStateRecord,
		facts map[string]json.RawMessage, inventory []InventoryHost) (uint64, error) {
		// The queue worker finalizes runs; mark them completed so retention
		// may consider them.
		rec.StartedAt = rec.FinishedAt.Add(-10 * time.Second)
		rec.State = RunStateCompleted
		in := RunInput{Run: rec, Hosts: hosts, Tasks: tasks, Facts: facts, Inventory: inventory}
		if err := d.CreateRun(ctx, &in.Run); err != nil {
			return 0, err
		}
		if err := d.SaveRun(ctx, in); err != nil {
			return 0, err
		}
		return in.Run.ID, nil
	}

	// Run 1: two hosts in one context.
	r1, err := save(
		RunRecord{Target: "home", Playbook: "site.yml", Inventory: "inv/one",
			FinishedAt: t1, DurationMS: 10000, ExitCode: 0, Success: true,
			Hosts: 2, ChangedHosts: 1, Output: "PLAY [all] ***\nok: [db1]"},
		[]HostRunRecord{
			{Target: "home", Host: "web1", Status: "changed", Ok: 10, Changed: 3},
			{Target: "home", Host: "db1", Status: "ok", Ok: 10},
		},
		[]TaskStateRecord{
			{Target: "home", Host: "web1", Task: "install nginx", Status: "changed", LastRunAt: t1, LastDiff: "+nginx"},
			{Target: "home", Host: "db1", Task: "start postgres", Status: "ok", LastRunAt: t1},
		},
		map[string]json.RawMessage{"web1": json.RawMessage(`{"os":"linux"}`)},
		[]InventoryHost{
			{Name: "web1", IP: "10.0.0.1", User: "deploy", Groups: []string{"webservers"}},
			{Name: "db1", IP: "10.0.0.2", User: "deploy", Groups: []string{"databases"}},
		},
	)
	if err != nil {
		t.Fatalf("save run 1: %v", err)
	}

	// Run 2: same hostname, different playbook context, with refreshed facts.
	r2, err := save(
		RunRecord{Target: "home", Playbook: "site2.yml", Inventory: "inv/two",
			FinishedAt: t2, DurationMS: 5000, ExitCode: 0, Success: true, Hosts: 1},
		[]HostRunRecord{{Target: "home", Host: "web1", Status: "ok", Ok: 5}},
		nil,
		map[string]json.RawMessage{"web1": json.RawMessage(`{"os":"linux","kernel":"6.1"}`)},
		[]InventoryHost{{Name: "web1", IP: "10.0.0.1", User: "deploy"}},
	)
	if err != nil {
		t.Fatalf("save run 2: %v", err)
	}

	// Run 3: back in run 1's context; db1 is recap'd but missing from the
	// inventory snapshot, so it leaves the inventory.
	r3, err := save(
		RunRecord{Target: "home", Playbook: "site.yml", Inventory: "inv/one",
			FinishedAt: t3, DurationMS: 7000, ExitCode: 2, Success: false,
			Hosts: 1, FailedHosts: 1},
		[]HostRunRecord{{Target: "home", Host: "db1", Status: "failed", Ok: 4, Failed: 1}},
		[]TaskStateRecord{
			{Target: "home", Host: "db1", Task: "migrate schema", Status: "failed", LastRunAt: t3, LastError: "boom"},
		},
		nil,
		[]InventoryHost{{Name: "web1", IP: "10.0.0.1", User: "deploy"}},
	)
	if err != nil {
		t.Fatalf("save run 3: %v", err)
	}

	runs, err := d.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 || runs[0].ID != r3 || runs[2].ID != r1 {
		t.Fatalf("ListRuns: want 3 runs newest first (r3..r1), got %v", runs)
	}
	full, err := d.Run(ctx, r1)
	if err != nil {
		t.Fatalf("Run(r1): %v", err)
	}
	if !strings.Contains(full.Output, "PLAY [all]") {
		t.Fatalf("Run(r1): output not round-tripped, got %q", full.Output)
	}

	hosts, err := d.ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("ListHosts: want 2 (one row per target+hostname), got %d: %+v", len(hosts), hosts)
	}
	byName := map[string]HostRecord{}
	for _, h := range hosts {
		if h.Target != "home" {
			t.Fatalf("ListHosts: host %s has target %q, want home", h.Name, h.Target)
		}
		byName[h.Name] = h
	}
	web1, ok := byName["web1"]
	if !ok {
		t.Fatalf("ListHosts: web1 missing: %+v", hosts)
	}
	if !web1.LastRunAt.Equal(t2) || web1.LastStatus != "ok" || !web1.InInventory {
		t.Fatalf("ListHosts: web1 should show its latest appearance (run 2), got %+v", web1)
	}
	// Facts live on the latest row, i.e. the one run 2 wrote.
	if string(web1.Facts) != `{"os":"linux","kernel":"6.1"}` {
		t.Fatalf("ListHosts: web1 facts not round-tripped, got %s", web1.Facts)
	}
	db1, ok := byName["db1"]
	if !ok {
		t.Fatalf("ListHosts: db1 missing: %+v", hosts)
	}
	if !db1.LastRunAt.Equal(t3) || db1.LastStatus != "failed" || db1.InInventory {
		t.Fatalf("ListHosts: db1 should be failed and out of inventory, got %+v", db1)
	}
	if len(db1.Facts) != 0 {
		t.Fatalf("ListHosts: db1 never reported facts, got %s", db1.Facts)
	}

	// Two context rows share the hostname; the run history must list each run
	// once, newest first, without duplicates.
	webRuns, err := d.HostRuns(ctx, "web1", 0)
	if err != nil {
		t.Fatalf("HostRuns(web1): %v", err)
	}
	if len(webRuns) != 2 || webRuns[0].RunID != r2 || webRuns[1].RunID != r1 {
		t.Fatalf("HostRuns(web1): want r2 then r1, got %+v", webRuns)
	}
	dbRuns, err := d.HostRuns(ctx, "db1", 0)
	if err != nil {
		t.Fatalf("HostRuns(db1): %v", err)
	}
	if len(dbRuns) != 2 || dbRuns[0].RunID != r3 || dbRuns[1].RunID != r1 {
		t.Fatalf("HostRuns(db1): want r3 then r1, got %+v", dbRuns)
	}

	tasks, err := d.RunTasks(ctx, r1)
	if err != nil {
		t.Fatalf("RunTasks(r1): %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("RunTasks(r1): want 2 tasks, got %d", len(tasks))
	}
	changes, err := d.HostChanges(ctx, "db1", 10)
	if err != nil {
		t.Fatalf("HostChanges(db1): %v", err)
	}
	foundMigrate := false
	for _, c := range changes {
		if c.Task == "migrate schema" && c.Status == "failed" {
			foundMigrate = true
		}
	}
	if !foundMigrate {
		t.Fatalf("HostChanges(db1): want the failed migrate task, got %+v", changes)
	}

	// Retention: keep the two newest runs, then drop hosts out of inventory.
	pruned, err := d.PruneRuns(ctx, "home", RetentionPolicy{MaxCount: 2})
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}
	if pruned.Runs != 1 {
		t.Fatalf("PruneRuns: want 1 run removed, got %+v", pruned)
	}
	runs, err = d.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns after prune: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("ListRuns after prune: want 2 runs, got %d", len(runs))
	}
	webRuns, err = d.HostRuns(ctx, "web1", 0)
	if err != nil {
		t.Fatalf("HostRuns(web1) after prune: %v", err)
	}
	if len(webRuns) != 1 || webRuns[0].RunID != r2 {
		t.Fatalf("HostRuns(web1) after prune: want only r2, got %+v", webRuns)
	}
	removed, err := d.PruneHosts(ctx, "home", time.Hour)
	if err != nil {
		t.Fatalf("PruneHosts: %v", err)
	}
	if removed != 1 {
		t.Fatalf("PruneHosts: want 1 host removed (db1), got %d", removed)
	}
	hosts, err = d.ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts after prune: %v", err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web1" {
		t.Fatalf("ListHosts after prune: want only web1, got %+v", hosts)
	}
}
