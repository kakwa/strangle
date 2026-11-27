package playbook_applier_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/playbook_applier"
	"github.com/kakwa/strangle/store"
	"github.com/kakwa/strangle/tests/testutil"
	"go.uber.org/zap"
)

// openDBWithDSN opens a private PostgreSQL database and also returns its DSN,
// so a test can inspect the GORM tables directly.
func openDBWithDSN(t *testing.T, label string) (*store.DB, string) {
	t.Helper()
	dsn := testutil.TestDSN(t, label)
	d, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("store.Open() error: %v", err)
	}
	return d, dsn
}

// TestResultsUseGormTables verifies that an applier run persists its outcome
// through store.Store into the existing run-history tables. River is not
// involved here and never owns job results.
func TestResultsUseGormTables(t *testing.T) {
	const output = `TASK [Install nginx] *******
--- before
+++ after
@@ -1 +1 @@
-old
+new
changed: [web1]

PLAY RECAP *******
web1 : ok=1    changed=1    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0
`
	st, dsn := openDBWithDSN(t, "applier-results")
	defer st.Close()
	ctx := context.Background()
	sqlDB := testutil.SQLDB(t, dsn)

	apr := playbook_applier.NewAnsible(
		[]cfg.TargetConfig{testTarget(t.TempDir())},
		executorFunc(func(context.Context, string, string, string, ...string) (int, string, map[string]json.RawMessage, error) {
			return 0, output, nil, nil
		}),
		st, zap.NewNop(),
	)
	if err := apr.Trigger(ctx, "prod", "site"); err != nil {
		t.Fatalf("Trigger() error: %v", err)
	}

	var runID uint64
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM runs WHERE target = $1 AND playbook = $2`, "prod", "site").Scan(&runID); err != nil {
		t.Fatalf("read run: %v", err)
	}
	for _, tc := range []struct {
		table string
		where string
	}{
		{"host_runs", "run_id"},
		{"task_run_results", "run_id"},
		{"task_states", "last_run_id"},
	} {
		var count int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM `+tc.table+` WHERE `+tc.where+` = $1`, runID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", tc.table, err)
		}
		if count != 1 {
			t.Errorf("%s has %d rows for run %d, want 1", tc.table, count, runID)
		}
	}
}
