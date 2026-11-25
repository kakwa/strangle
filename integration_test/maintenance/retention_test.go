package maintenance_test

import (
	"context"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/cfg"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/maintenance"
	"github.com/kakwa/strangle/internal/testutil"
	"go.uber.org/zap"
)

func seedRun(t *testing.T, st *db.DB, id uint64, target string, finished time.Time) {
	t.Helper()
	run := db.RunRecord{
		ID: id, Target: target, Playbook: "site",
		StartedAt: finished.Add(-time.Minute), FinishedAt: finished,
		State: db.RunStateCompleted, Success: true,
	}
	if err := st.SaveRun(context.Background(), db.RunInput{Run: run}); err != nil {
		t.Fatalf("SaveRun(%d) error: %v", id, err)
	}
}

func TestRetentionPrunePolicies(t *testing.T) {
	st := testutil.OpenDB(t, "maintenance")
	defer st.Close()
	now := time.Now().UTC()

	seedRun(t, st, 1, "home", now.Add(-72*time.Hour))
	seedRun(t, st, 2, "home", now.Add(-time.Hour))
	seedRun(t, st, 3, "edge", now.Add(-2*time.Hour))
	seedRun(t, st, 4, "edge", now.Add(-time.Hour))

	targets := []cfg.TargetConfig{
		{Name: "home"},
		{Name: "edge", Retention: cfg.RetentionConfig{MaxCount: 1}},
	}
	m := maintenance.NewRetention(st, cfg.RetentionConfig{MaxAge: 24 * time.Hour, Interval: time.Minute},
		func() []cfg.TargetConfig { return targets }, zap.NewNop())
	if m == nil {
		t.Fatal("maintenance.NewRetention() = nil, want an enabled task")
	}
	m.Once(context.Background())

	runs, err := st.ListRuns(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRuns() error: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("remaining runs = %v, want home #2 and edge #4", runs)
	}
	for _, r := range runs {
		if r.ID != 2 && r.ID != 4 {
			t.Errorf("unexpected remaining run %d", r.ID)
		}
	}
}

func TestRetentionDisabled(t *testing.T) {
	st := testutil.OpenDB(t, "maintenance")
	defer st.Close()
	targets := []cfg.TargetConfig{{Name: "home"}}
	if m := maintenance.NewRetention(st, cfg.RetentionConfig{}, func() []cfg.TargetConfig { return targets }, zap.NewNop()); m != nil {
		t.Fatal("maintenance.NewRetention() enabled retention with no policy")
	}
	// A nil task is safe for callers that do not check the constructor result.
	var m *maintenance.Task
	m.Once(context.Background())
	m.Run(context.Background())
}
