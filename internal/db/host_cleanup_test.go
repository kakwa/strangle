package db

import (
	"context"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/store"
)

func seedDBHostRun(t *testing.T, d *DB, id uint64, target string, finished time.Time, inventory []store.InventoryHost, hosts []string) {
	t.Helper()
	run := store.RunRecord{
		ID: id, Target: target, Playbook: "site",
		StartedAt: finished.Add(-time.Minute), FinishedAt: finished,
		State: store.RunStateCompleted, Success: true,
	}
	recaps := make([]store.HostRunRecord, 0, len(hosts))
	tasks := make([]store.TaskStateRecord, 0, len(hosts))
	for _, name := range hosts {
		recaps = append(recaps, store.HostRunRecord{Host: name, Ok: 1})
		tasks = append(tasks, store.TaskStateRecord{Host: name, Task: "install", Status: "ok", LastRunID: id})
	}
	if err := d.SaveRun(context.Background(), store.RunInput{
		Run: run, Hosts: recaps, Tasks: tasks, Inventory: inventory,
	}); err != nil {
		t.Fatalf("SaveRun(%d) error: %v", id, err)
	}
}

func TestDBPruneHosts(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedDBHostRun(t, d, 1, "home", now.Add(-72*time.Hour),
		[]store.InventoryHost{{Name: "old"}, {Name: "recent"}}, []string{"old", "recent"})
	seedDBHostRun(t, d, 2, "home", now.Add(-time.Hour),
		[]store.InventoryHost{{Name: "recent"}}, []string{"recent"})
	seedDBHostRun(t, d, 3, "edge", now.Add(-72*time.Hour),
		[]store.InventoryHost{{Name: "old"}}, []string{"old"})

	// Emulate the collection dropping the host from its inventory. The
	// in-memory test mutates its map directly; here the flag is a column.
	var staleIDs []uint64
	if err := d.db.WithContext(ctx).Model(&Host{}).
		Where("target = ? AND name = ?", "home", "old").Pluck("id", &staleIDs).Error; err != nil {
		t.Fatalf("find stale host: %v", err)
	}
	if len(staleIDs) != 1 {
		t.Fatalf("stale host IDs = %v, want one", staleIDs)
	}
	if err := d.db.WithContext(ctx).Model(&Host{}).
		Where("id = ?", staleIDs[0]).Update("in_inventory", false).Error; err != nil {
		t.Fatalf("mark host out of inventory: %v", err)
	}

	removed, err := d.PruneHosts(ctx, "home", 24*time.Hour)
	if err != nil {
		t.Fatalf("PruneHosts() error: %v", err)
	}
	if removed != 1 {
		t.Fatalf("PruneHosts() = %d, want 1", removed)
	}

	var names []string
	if err := d.db.WithContext(ctx).Model(&Host{}).Pluck("name", &names).Error; err != nil {
		t.Fatalf("list hosts: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("remaining host names = %v, want two", names)
	}

	var oldIDs []uint64
	if err := d.db.WithContext(ctx).Model(&Host{}).
		Where("target = ? AND name = ?", "home", "old").Pluck("id", &oldIDs).Error; err != nil {
		t.Fatalf("find removed host: %v", err)
	}
	if len(oldIDs) != 0 {
		t.Fatal("stale home host still exists")
	}
	// The removed host's recap and task rows must not survive as orphans.
	for _, model := range []any{&HostRun{}, &TaskRunResult{}, &TaskState{}} {
		var count int64
		if err := d.db.WithContext(ctx).Model(model).
			Where("host_id = ?", staleIDs[0]).Count(&count).Error; err != nil {
			t.Fatalf("count child rows: %v", err)
		}
		if count != 0 {
			t.Errorf("host %d left %d child rows in %T", staleIDs[0], count, model)
		}
	}
	// The edge host with the same name is untouched.
	runs, err := d.HostRuns(ctx, "old", 10)
	if err != nil || len(runs) != 1 || runs[0].Target != "edge" {
		t.Fatalf("HostRuns(old) = %v, %v; want only the edge run", runs, err)
	}
}
