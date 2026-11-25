package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/testutil"
	"github.com/kakwa/strangle/internal/worker"
	"go.uber.org/zap"
)

type stubWorkers struct{ statuses []worker.WorkerStatus }

func (s stubWorkers) WorkerStatuses(context.Context) []worker.WorkerStatus { return s.statuses }

type failingStore struct{ db.Store }

func (failingStore) Stats(context.Context) (*db.Stats, error) {
	return nil, errors.New("database unavailable")
}

func getMetrics(t *testing.T, s *server.Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec
}

func TestHandlerRendersStoreAndWorkerMetrics(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	for i, success := range []bool{true, false} {
		run := db.RunRecord{
			ID: uint64(i + 1), Target: "home\nlab", Playbook: "site",
			StartedAt: now.Add(-time.Minute), FinishedAt: now,
			DurationMS: 1500, Success: success,
		}
		if !success {
			run.State = db.RunStateCompleted
		}
		if err := st.SaveRun(ctx, db.RunInput{
			Run:       run,
			Hosts:     []db.HostRunRecord{{Host: "web1", Ok: 1}},
			Inventory: []db.InventoryHost{{Name: "web1"}},
		}); err != nil {
			t.Fatalf("SaveRun() error: %v", err)
		}
	}

	workers := stubWorkers{statuses: []worker.WorkerStatus{
		{ID: 2, Collection: "edge", Alive: false, Jobs: 7, Restarts: 1},
		{ID: 1, Alive: true, Jobs: 3},
	}}
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	s := server.New("test", apr, st, nil, nil, workers, nil, nil, nil, "", zap.NewNop())

	rec := getMetrics(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q, want Prometheus text format", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE strangle_runs gauge",
		"strangle_runs 2\n",
		"strangle_runs_succeeded 1\n",
		"strangle_runs_failed 1\n",
		"strangle_run_duration_ms_avg 1500\n",
		`strangle_playbook_runs{collection="home\nlab",playbook="site"} 2`,
		`strangle_playbook_failed_runs{collection="home\nlab",playbook="site"} 1`,
		`strangle_workers{state="total"} 2`,
		`strangle_workers{state="alive"} 1`,
		`strangle_worker_jobs{worker="2",collection="edge"} 7`,
		"# TYPE go_goroutines gauge",
		"strangle_build_info{version=\"dev\"} 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output does not contain %q:\n%s", want, body)
		}
	}
}

func TestHandlerStoreFailureStillServesRuntimeMetrics(t *testing.T) {
	s := server.New("test", nil, failingStore{}, nil, nil, nil, nil, nil, nil, "", zap.NewNop())

	rec := getMetrics(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "strangle_store_up 0") {
		t.Error("store failure was not reported by strangle_store_up")
	}
	if !strings.Contains(body, "strangle_workers{state=\"total\"} 0") {
		t.Error("missing zero-valued worker metrics")
	}
	if !strings.Contains(body, "# TYPE go_goroutines gauge") {
		t.Error("missing Go runtime metrics")
	}
}
