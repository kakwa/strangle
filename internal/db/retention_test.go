package db

import (
	"context"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/store"
)

func seedDBRetentionRun(t *testing.T, d *DB, id uint64, target, state string, finished time.Time) {
	t.Helper()
	ctx := context.Background()
	in := store.RunInput{
		Run: store.RunRecord{
			ID: id, Target: target, Playbook: "site",
			StartedAt: finished.Add(-time.Minute), FinishedAt: finished,
			State: state, Success: state != store.RunStateCancelled,
		},
		Hosts: []store.HostRunRecord{{Host: "h1", Ok: 1}},
		Tasks: []store.TaskStateRecord{{Host: "h1", Task: "install", Status: "ok", LastRunID: id}},
	}
	if err := d.SaveRun(ctx, in); err != nil {
		t.Fatalf("SaveRun(%d) error: %v", id, err)
	}
}

func TestDBPruneRuns(t *testing.T) {
	d := openTestDB(t)
	defer d.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedDBRetentionRun(t, d, 1, "home", store.RunStateCompleted, now.Add(-48*time.Hour))
	seedDBRetentionRun(t, d, 2, "home", store.RunStateCompleted, now.Add(-time.Hour))
	seedDBRetentionRun(t, d, 3, "home", store.RunStateRunning, now.Add(-72*time.Hour))
	seedDBRetentionRun(t, d, 4, "edge", store.RunStateCompleted, now.Add(-72*time.Hour))

	res, err := d.PruneRuns(ctx, "home", store.RetentionPolicy{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatalf("PruneRuns() error: %v", err)
	}
	if res.Runs != 1 || res.HostRuns != 1 || res.TaskResults != 1 {
		t.Fatalf("PruneRuns() = %+v, want one run and one row of each child type", res)
	}

	for _, id := range []uint64{2, 3, 4} {
		if _, err := d.Run(ctx, id); err != nil {
			t.Errorf("run %d should be retained: %v", id, err)
		}
	}
	if _, err := d.Run(ctx, 1); err == nil {
		t.Error("old run was not purged")
	}

	for _, table := range []struct {
		name  string
		model any
	}{
		{"host_runs", &HostRun{}},
		{"task_run_results", &TaskRunResult{}},
	} {
		var count int64
		if err := d.db.WithContext(ctx).Model(table.model).Where("run_id = ?", 1).Count(&count).Error; err != nil {
			t.Fatalf("count %s: %v", table.name, err)
		}
		if count != 0 {
			t.Errorf("%s has %d orphaned rows for purged run", table.name, count)
		}
	}
	var lastRunIDs []uint64
	if err := d.db.WithContext(ctx).Model(&TaskState{}).
		Where("task = ?", "install").Pluck("last_run_id", &lastRunIDs).Error; err != nil {
		t.Fatalf("read task state: %v", err)
	}
	for _, id := range lastRunIDs {
		if id == 1 {
			t.Error("task state still links to the purged run")
		}
	}
}
