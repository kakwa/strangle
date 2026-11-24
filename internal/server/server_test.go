package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
)

// stubExecutor satisfies applier.Executor; the /api/run tests never execute
// a playbook, they only check that the handler routes to the enqueuer.
type stubExecutor struct{}

func (stubExecutor) RunPlaybook(context.Context, string, string, ...string) (int, string, error) {
	return 0, "", nil
}

// recEnqueuer records every EnqueueRun call and returns a canned response.
type recEnqueuer struct {
	mu     sync.Mutex
	calls  []string
	status string
	err    error
}

func (r *recEnqueuer) EnqueueRun(_ context.Context, target, playbook string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, target+"/"+playbook)
	return r.status, r.err
}

func (r *recEnqueuer) called() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recEnqueuer) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return ""
	}
	return r.calls[len(r.calls)-1]
}

// newTestServer builds a Server with one configured target and playbook.
func newTestServer(enq RunEnqueuer) *Server {
	targets := []config.TargetConfig{{
		Name: "home",
		Playbooks: []config.PlaybookConfig{
			{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
		},
	}}
	apr := applier.NewAnsible(targets, stubExecutor{}, store.NewMem(), zap.NewNop())
	return New("test", apr, store.NewMem(), nil, enq, zap.NewNop())
}

func postRun(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func TestPostRunEnqueues(t *testing.T) {
	enq := &recEnqueuer{status: "queued"}
	s := newTestServer(enq)

	rec := postRun(t, s, `{"target":"home","playbook":"site"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if enq.called() != 1 || enq.last() != "home/site" {
		t.Fatalf("enqueuer calls = %v, want one call home/site", enq.calls)
	}

	var out struct {
		Status   string `json:"status"`
		Target   string `json:"target"`
		Playbook string `json:"playbook"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Status != "queued" || out.Target != "home" || out.Playbook != "site" {
		t.Errorf("response = %+v, want queued home/site", out)
	}
}

func TestPostRunEnqueueError(t *testing.T) {
	enq := &recEnqueuer{err: errors.New("db down")}
	s := newTestServer(enq)

	rec := postRun(t, s, `{"target":"home","playbook":"site"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(rec.Body.String(), "db down") {
		t.Errorf("body = %s, want it to contain the enqueue error", rec.Body.String())
	}
}

func TestPostRunUnknownPlaybook(t *testing.T) {
	enq := &recEnqueuer{status: "queued"}
	s := newTestServer(enq)

	rec := postRun(t, s, `{"target":"home","playbook":"nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if enq.called() != 0 {
		t.Fatal("enqueuer must not be called for an unknown playbook")
	}
}

func TestPostRunMissingFields(t *testing.T) {
	enq := &recEnqueuer{status: "queued"}
	s := newTestServer(enq)

	for _, body := range []string{`{}`, `{"target":"home"}`, `{"playbook":"site"}`, `not json`} {
		rec := postRun(t, s, body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}
	if enq.called() != 0 {
		t.Fatal("enqueuer must not be called for invalid requests")
	}
}
