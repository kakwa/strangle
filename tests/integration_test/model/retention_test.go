package model_test

import (
	"context"
	"testing"
	"time"

	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/tests/testutil"
)

func seedDBRetentionRun(t *testing.T, d *model.DB, id uint64, target, state string, finished time.Time) {
	t.Helper()
	ctx := context.Background()
	in := model.RunInput{
		Run: model.RunRecord{
			ID: id, Target: target, Playbook: "site",
			StartedAt: finished.Add(-time.Minute), FinishedAt: finished,
			State: state, Success: state != model.RunStateCancelled,
		},
		Hosts: []model.HostRunRecord{{Host: "h1", Ok: 1}},
		Tasks: []model.TaskStateRecord{{Host: "h1", Task: "install", Status: "ok", LastRunID: id}},
	}
	if err := d.SaveRun(ctx, in); err != nil {
		t.Fatalf("SaveRun(%d) error: %v", id, err)
	}
}

func TestDBPruneRuns(t *testing.T) {
	d, dsn := openTestDB(t)
	defer d.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedDBRetentionRun(t, d, 1, "home", model.RunStateCompleted, now.Add(-48*time.Hour))
	seedDBRetentionRun(t, d, 2, "home", model.RunStateCompleted, now.Add(-time.Hour))
	seedDBRetentionRun(t, d, 3, "home", model.RunStateRunning, now.Add(-72*time.Hour))
	seedDBRetentionRun(t, d, 4, "edge", model.RunStateCompleted, now.Add(-72*time.Hour))

	res, err := d.PruneRuns(ctx, "home", model.RetentionPolicy{MaxAge: 24 * time.Hour})
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

	sqlDB := testutil.SQLDB(t, dsn)
	for _, table := range []string{"host_runs", "task_run_results"} {
		var count int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM `+table+` WHERE run_id = $1`, 1).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s has %d orphaned rows for purged run", table, count)
		}
	}
	var purgedLinks int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM task_states WHERE last_run_id = $1`, 1).Scan(&purgedLinks); err != nil {
		t.Fatalf("read task state: %v", err)
	}
	if purgedLinks != 0 {
		t.Error("task state still links to the purged run")
	}
}
