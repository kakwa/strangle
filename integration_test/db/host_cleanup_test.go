package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/internal/testutil"
)

func seedDBHostRun(t *testing.T, d *db.DB, id uint64, target string, finished time.Time, inventory []store.InventoryHost, hosts []string) {
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
	d, dsn := openTestDB(t)
	defer d.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedDBHostRun(t, d, 1, "home", now.Add(-72*time.Hour),
		[]store.InventoryHost{{Name: "old"}, {Name: "recent"}}, []string{"old", "recent"})
	seedDBHostRun(t, d, 2, "home", now.Add(-time.Hour),
		[]store.InventoryHost{{Name: "recent"}}, []string{"recent"})
	seedDBHostRun(t, d, 3, "edge", now.Add(-72*time.Hour),
		[]store.InventoryHost{{Name: "old"}}, []string{"old"})

	// Emulate the collection dropping the host from its inventory: the flag
	// is a column in PostgreSQL.
	sqlDB := testutil.SQLDB(t, dsn)
	var staleID uint64
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM hosts WHERE target = $1 AND name = $2`, "home", "old").Scan(&staleID); err != nil {
		t.Fatalf("find stale host: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE hosts SET in_inventory = false WHERE id = $1`, staleID); err != nil {
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
	rows, err := sqlDB.QueryContext(ctx, `SELECT name FROM hosts`)
	if err != nil {
		t.Fatalf("list hosts: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatalf("scan host: %v", err)
		}
		names = append(names, name)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate hosts: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("remaining host names = %v, want two", names)
	}

	var oldCount int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM hosts WHERE target = $1 AND name = $2`, "home", "old").Scan(&oldCount); err != nil {
		t.Fatalf("find removed host: %v", err)
	}
	if oldCount != 0 {
		t.Fatal("stale home host still exists")
	}
	// The removed host's recap and task rows must not survive as orphans.
	for _, table := range []string{"host_runs", "task_run_results", "task_states"} {
		var count int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE host_id = $1`, staleID).Scan(&count); err != nil {
			t.Fatalf("count child rows in %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("host %d left %d child rows in %s", staleID, count, table)
		}
	}
	// The edge host with the same name is untouched.
	runs, err := d.HostRuns(ctx, "old", 10)
	if err != nil || len(runs) != 1 || runs[0].Target != "edge" {
		t.Fatalf("HostRuns(old) = %v, %v; want only the edge run", runs, err)
	}
}
