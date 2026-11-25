package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/worker"
	"go.uber.org/zap"
)

type stubWorkers struct{ statuses []worker.WorkerStatus }

func (s stubWorkers) WorkerStatuses(context.Context) []worker.WorkerStatus { return s.statuses }

func getMetrics(t *testing.T, s *server.Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec
}

func TestHandlerRendersWorkerMetrics(t *testing.T) {
	workers := stubWorkers{statuses: []worker.WorkerStatus{
		{ID: 2, Collection: "edge", Alive: false, Jobs: 7, Restarts: 1},
		{ID: 1, Alive: true, Jobs: 3},
	}}
	s := server.New("test", nil, nil, nil, nil, workers, nil, nil, nil, "", zap.NewNop())

	rec := getMetrics(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type = %q, want Prometheus text format", got)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`strangle_workers{state="total"} 2`,
		`strangle_workers{state="alive"} 1`,
		`strangle_workers{state="dead"} 1`,
		`strangle_worker_jobs{worker="2",collection="edge"} 7`,
		`strangle_worker_restarts_total{worker="2",collection="edge"} 1`,
		"# TYPE go_goroutines gauge",
		"strangle_build_info{version=\"dev\"} 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output does not contain %q:\n%s", want, body)
		}
	}
}

func TestHandlerWithoutWorkersStillServesRuntimeMetrics(t *testing.T) {
	s := server.New("test", nil, nil, nil, nil, nil, nil, nil, nil, "", zap.NewNop())

	rec := getMetrics(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `strangle_workers{state="total"} 0`) {
		t.Error("missing zero-valued worker metrics")
	}
	if !strings.Contains(body, "# TYPE go_goroutines gauge") {
		t.Error("missing Go runtime metrics")
	}
}
