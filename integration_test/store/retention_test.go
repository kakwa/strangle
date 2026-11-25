package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/db"
)

func seedRetentionRun(t *testing.T, st *db.DB, id uint64, target, state string, finished time.Time) {
	t.Helper()
	run := testRun(target, "site", finished)
	run.ID = id
	run.State = state
	if err := st.SaveRun(context.Background(), db.RunInput{
		Run:   run,
		Hosts: []db.HostRunRecord{{Host: "h1", Ok: 1}},
		Tasks: []db.TaskStateRecord{{Host: "h1", Task: "install", Status: "ok", LastRunID: id}},
	}); err != nil {
		t.Fatalf("SaveRun(%d) error: %v", id, err)
	}
}

func TestStorePruneRunsMaxAge(t *testing.T) {
	st := openStore(t)
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedRetentionRun(t, st, 1, "home", db.RunStateCompleted, now.Add(-48*time.Hour))
	seedRetentionRun(t, st, 2, "home", db.RunStateCompleted, now.Add(-time.Hour))
	seedRetentionRun(t, st, 3, "home", db.RunStateRunning, now.Add(-72*time.Hour))
	seedRetentionRun(t, st, 4, "edge", db.RunStateCompleted, now.Add(-72*time.Hour))

	res, err := st.PruneRuns(ctx, "home", db.RetentionPolicy{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatalf("PruneRuns() error: %v", err)
	}
	if res.Runs != 1 || res.HostRuns != 1 || res.TaskResults != 1 {
		t.Fatalf("PruneRuns() = %+v, want one run and one row of each child type", res)
	}

	runs, err := st.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns() error: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("remaining runs = %v, want 3", runs)
	}
	for _, r := range runs {
		if r.ID == 1 {
			t.Error("old completed run was not purged")
		}
	}
	// In-flight and other collections' runs are never touched.
	if _, err := st.Run(ctx, 3); err != nil {
		t.Errorf("running run was purged: %v", err)
	}
	if _, err := st.Run(ctx, 4); err != nil {
		t.Errorf("other collection's run was purged: %v", err)
	}

	taskRuns, err := st.RunTasks(ctx, 1)
	if len(taskRuns) != 0 || (err != nil && !errors.Is(err, db.ErrNotFound)) {
		t.Errorf("RunTasks(1) = %v, %v; want no orphaned results", taskRuns, err)
	}
	changes, err := st.HostChanges(ctx, "h1", 10)
	if err != nil {
		t.Fatalf("HostChanges() error: %v", err)
	}
	for _, change := range changes {
		if change.LastRunID == 1 {
			t.Error("latest task state still points at the purged run")
		}
	}
}

func TestStorePruneRunsMaxCount(t *testing.T) {
	st := openStore(t)
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	for id := uint64(1); id <= 4; id++ {
		seedRetentionRun(t, st, id, "home", db.RunStateCompleted, now.Add(time.Duration(id)*time.Hour))
	}
	res, err := st.PruneRuns(ctx, "home", db.RetentionPolicy{MaxCount: 2})
	if err != nil {
		t.Fatalf("PruneRuns() error: %v", err)
	}
	if res.Runs != 2 {
		t.Fatalf("PruneRuns() = %+v, want 2 runs removed", res)
	}
	runs, err := st.ListRuns(ctx, 10)
	if err != nil {
		t.Fatalf("ListRuns() error: %v", err)
	}
	if len(runs) != 2 || runs[0].ID != 4 || runs[1].ID != 3 {
		t.Fatalf("remaining runs = %v, want the two newest", runs)
	}

	// A disabled policy is a no-op.
	before := len(runs)
	res, err = st.PruneRuns(ctx, "home", db.RetentionPolicy{})
	if err != nil || res.Runs != 0 {
		t.Fatalf("disabled PruneRuns() = %+v, %v", res, err)
	}
	runs, _ = st.ListRuns(ctx, 10)
	if len(runs) != before {
		t.Errorf("disabled policy removed runs: %v", runs)
	}
}
