package metrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/internal/worker"
	"go.uber.org/zap"
)

type stubWorkers struct{ statuses []worker.WorkerStatus }

func (s stubWorkers) WorkerStatuses(context.Context) []worker.WorkerStatus { return s.statuses }

type failingStore struct{ store.Store }

func (failingStore) Stats(context.Context) (*store.Stats, error) {
	return nil, errors.New("database unavailable")
}

func TestHandlerRendersStoreAndWorkerMetrics(t *testing.T) {
	st := store.NewMem()
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	for i, success := range []bool{true, false} {
		run := store.RunRecord{
			ID: uint64(i + 1), Target: "home\nlab", Playbook: "site",
			StartedAt: now.Add(-time.Minute), FinishedAt: now,
			DurationMS: 1500, Success: success,
		}
		if !success {
			run.State = store.RunStateCompleted
		}
		if err := st.SaveRun(ctx, store.RunInput{
			Run:       run,
			Hosts:     []store.HostRunRecord{{Host: "web1", Ok: 1}},
			Inventory: []store.InventoryHost{{Name: "web1"}},
		}); err != nil {
			t.Fatalf("SaveRun() error: %v", err)
		}
	}

	workers := stubWorkers{statuses: []worker.WorkerStatus{
		{ID: 2, Collection: "edge", Alive: false, Jobs: 7, Restarts: 1},
		{ID: 1, Alive: true, Jobs: 3},
	}}
	rec := httptest.NewRecorder()
	Handler(st, workers, zap.NewNop()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != contentType {
		t.Errorf("Content-Type = %q, want %q", got, contentType)
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
	rec := httptest.NewRecorder()
	Handler(failingStore{}, nil, zap.NewNop()).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/metrics", nil))

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
