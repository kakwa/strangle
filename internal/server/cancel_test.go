package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kakwa/strangle/internal/store"
)

func seedRunWithState(t *testing.T, st store.Store, id uint64, target, state, jobID string) {
	t.Helper()
	if err := st.SaveRun(context.Background(), store.RunInput{
		Run: store.RunRecord{
			ID: id, Target: target, Playbook: "site", State: state, JobID: jobID,
		},
	}); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
}

func postCancel(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func TestCancelRunVersionedCollectionScope(t *testing.T) {
	st := store.NewMem()
	seedRunWithState(t, st, 1, "home", store.RunStateRunning, "42")
	enq := &recEnqueuer{status: "queued"}
	s := newOrgTestServer(enq, st)

	rec := postCancel(t, s, "/api/v1/orgs/default/collections/home/runs/1/cancel")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := enq.canceled(); len(got) != 1 || got[0] != "42" {
		t.Fatalf("cancelled jobs = %v, want [42]", got)
	}
	run, err := st.Run(context.Background(), 1)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if run.State != store.RunStateCancelled || !run.Canceled {
		t.Fatalf("run state = %+v, want cancelled", run)
	}

	// A worker that finishes after the cancellation request must not turn the
	// record back into a completed run or erase its job ID.
	if err := st.SaveRun(context.Background(), store.RunInput{
		Run: store.RunRecord{ID: 1, Target: "home", Playbook: "site", State: store.RunStateCompleted, Success: true},
	}); err != nil {
		t.Fatalf("SaveRun() after cancel: %v", err)
	}
	run, err = st.Run(context.Background(), 1)
	if err != nil {
		t.Fatalf("Run(): %v", err)
	}
	if run.State != store.RunStateCancelled || !run.Canceled {
		t.Fatalf("run state after worker finish = %+v, want cancelled", run)
	}
	if run.JobID != "42" {
		t.Fatalf("job ID after worker finish = %q, want 42", run.JobID)
	}

	// No flat alias: a run belongs to a collection and must be acted on via
	// the collection-scoped URL. An unregistered API path falls through to
	// the SPA handler, which answers with HTML rather than JSON.
	if rec = postCancel(t, s, "/api/runs/1/cancel"); !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("flat cancel content type = %q, want the SPA fallback", rec.Header().Get("Content-Type"))
	}
	// A sibling collection cannot cancel another collection's run.
	if rec = postCancel(t, s, "/api/v1/orgs/lab/collections/edge/runs/1/cancel"); rec.Code != http.StatusNotFound {
		t.Errorf("sibling cancel status = %d, want 404", rec.Code)
	}
	if len(enq.canceled()) != 1 {
		t.Fatalf("unexpected extra cancellation: %v", enq.canceled())
	}
}

func TestCancelRunRejectsCompletedAndInvalid(t *testing.T) {
	st := store.NewMem()
	seedRunWithState(t, st, 1, "home", store.RunStateCompleted, "42")
	enq := &recEnqueuer{status: "queued"}
	s := newOrgTestServer(enq, st)

	for _, path := range []string{
		"/api/v1/orgs/default/collections/home/runs/1/cancel",
		"/api/v1/orgs/default/collections/home/runs/nope/cancel",
		"/api/v1/orgs/default/collections/home/runs/999/cancel",
	} {
		rec := postCancel(t, s, path)
		if rec.Code == http.StatusOK || rec.Code == http.StatusAccepted {
			t.Errorf("POST %s: status = %d, want an error", path, rec.Code)
		}
	}
	if len(enq.canceled()) != 0 {
		t.Fatalf("canceler called for an inactive run: %v", enq.canceled())
	}
}
