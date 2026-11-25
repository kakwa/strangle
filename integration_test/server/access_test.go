package server_test

import (
	"context"
	"encoding/json"
	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/testutil"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/cfg"
	"github.com/kakwa/strangle/internal/db"
	"go.uber.org/zap"
)

// newAccessTestServer builds an authenticated server with two collections
// and the supplied membership db.
func newAccessTestServer(st db.Store, users auth.UserStore, access auth.CollectionAccess, enq server.RunEnqueuer) *server.Server {
	targets := []cfg.TargetConfig{
		{Name: "home", Playbooks: []cfg.PlaybookConfig{{Name: "site"}}},
		{Name: "edge", Org: "lab", Playbooks: []cfg.PlaybookConfig{{Name: "site"}}},
	}
	apr := applier.NewAnsible(targets, stubExecutor{}, st, zap.NewNop())
	return server.New("test", apr, st, nil, enq, nil, nil, users, nil, "", zap.NewNop(),
		server.WithCollectionAccess(access))
}

func TestCollectionRestrictions(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	seedTargetRun(t, st, "home")
	seedTargetRun(t, st, "edge")
	seedRunWithState(t, st, 100, "edge", db.RunStateRunning, "99")
	seedUser(t, st.Users(), "alice", "pw")
	access := st.Access()
	if err := access.Add(context.Background(), "alice", "lab", "edge"); err != nil {
		t.Fatalf("Add(): %v", err)
	}
	enq := &recEnqueuer{status: "queued"}
	s := newAccessTestServer(st, st.Users(), access, enq)
	cookie := loginCookie(t, s, "alice", "pw")
	headers := map[string]string{"Cookie": cookie.String()}

	// Only the org/collection alice belongs to is listed.
	rec := getAuthWith(t, s, "/api/v1/orgs", headers)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET orgs: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var orgs []orgInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &orgs); err != nil {
		t.Fatalf("decode orgs: %v", err)
	}
	if len(orgs) != 1 || orgs[0].Name != "lab" || len(orgs[0].Collections) != 1 || orgs[0].Collections[0].Name != "edge" {
		t.Fatalf("visible orgs = %+v, want only lab/edge", orgs)
	}

	// Flat listings are filtered to the same set, so the UI cannot show
	// data from collections the user cannot access.
	for _, path := range []string{"/api/runs", "/api/hosts", "/api/playbooks"} {
		rec = getAuthWith(t, s, path, headers)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, body %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "home") {
			t.Errorf("GET %s exposes the home collection: %s", path, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "edge") {
			t.Errorf("GET %s does not expose the edge collection: %s", path, rec.Body.String())
		}
	}

	// Direct access to a foreign collection is denied; an allowed one works.
	if rec = getAuthWith(t, s, "/api/v1/orgs/default/collections/home", headers); rec.Code != http.StatusForbidden {
		t.Errorf("GET home: status = %d, want 403", rec.Code)
	}
	if rec = getAuthWith(t, s, "/api/v1/orgs/lab/collections/edge/runs", headers); rec.Code != http.StatusOK {
		t.Errorf("GET edge runs: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// Run details carry full output, so they must respect the same membership
	// rules as the listing.
	runs, err := st.ListRuns(context.Background(), 10)
	if err != nil || len(runs) != 3 {
		t.Fatalf("ListRuns() = %v, %v", runs, err)
	}
	ids := map[string]string{}
	for _, run := range runs {
		ids[run.Target] = strconv.FormatUint(run.ID, 10)
	}
	if rec = getAuthWith(t, s, "/api/runs/"+ids["home"], headers); rec.Code != http.StatusNotFound {
		t.Errorf("GET home run detail: status = %d, want 404", rec.Code)
	}
	if rec = getAuthWith(t, s, "/api/v1/orgs/default/collections/home/runs/"+ids["home"], headers); rec.Code != http.StatusForbidden {
		t.Errorf("GET home collection run detail: status = %d, want 403", rec.Code)
	}
	if rec = getAuthWith(t, s, "/api/v1/orgs/lab/collections/edge/runs/"+ids["edge"], headers); rec.Code != http.StatusOK {
		t.Errorf("GET edge run detail: status = %d, want 200", rec.Code)
	}

	// Per-run task history carries diffs/errors and follows the same rules.
	if rec = getAuthWith(t, s, "/api/v1/orgs/default/collections/home/runs/"+ids["home"]+"/tasks", headers); rec.Code != http.StatusForbidden {
		t.Errorf("GET home run tasks: status = %d, want 403", rec.Code)
	}
	if rec = getAuthWith(t, s, "/api/v1/orgs/lab/collections/edge/runs/"+ids["edge"]+"/tasks", headers); rec.Code != http.StatusOK {
		t.Errorf("GET edge run tasks: status = %d, want 200", rec.Code)
	}

	// POST /run is checked using the target in the body on flat routes.
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Cookie", cookie.String())
		out := httptest.NewRecorder()
		s.Routes().ServeHTTP(out, req)
		return out
	}
	if rec = post("/api/run", `{"target":"home","playbook":"site"}`); rec.Code != http.StatusForbidden {
		t.Errorf("POST home run: status = %d, want 403", rec.Code)
	}
	if rec = post("/api/run", `{"target":"edge","playbook":"site"}`); rec.Code != http.StatusAccepted {
		t.Errorf("POST edge run: status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if enq.called() != 1 || enq.last() != "edge/site" {
		t.Errorf("enqueuer calls = %v, want one edge/site", enq.calls)
	}

	// Cancel is collection-scoped: a foreign collection is denied before the
	// run is looked up.
	if rec = post("/api/v1/orgs/default/collections/home/runs/100/cancel", ""); rec.Code != http.StatusForbidden {
		t.Errorf("POST home run cancel: status = %d, want 403", rec.Code)
	}
	if len(enq.canceled()) != 0 {
		t.Fatalf("canceler called for a foreign collection: %v", enq.canceled())
	}

	// SSH host keys are per collection too: neither the view nor a decision
	// may reach a collection the user does not belong to.
	if rec = getAuthWith(t, s, "/api/v1/orgs/default/collections/home/hostkeys", headers); rec.Code != http.StatusForbidden {
		t.Errorf("GET home hostkeys: status = %d, want 403", rec.Code)
	}
	if rec = getAuthWith(t, s, "/api/v1/orgs/lab/collections/edge/hostkeys", headers); rec.Code != http.StatusOK {
		t.Errorf("GET edge hostkeys: status = %d, want 200", rec.Code)
	}
	if rec = getAuthWith(t, s, "/api/v1/orgs/default/collections/home/ssh/stats", headers); rec.Code != http.StatusForbidden {
		t.Errorf("GET home ssh stats: status = %d, want 403", rec.Code)
	}
	body := `{"target":"home/site","host":"web1","fingerprint":"SHA256:abc"}`
	if rec = post("/api/v1/orgs/default/collections/home/hostkeys/accept", body); rec.Code != http.StatusForbidden {
		t.Errorf("POST home hostkey accept: status = %d, want 403", rec.Code)
	}
	if rec = post("/api/v1/orgs/lab/collections/edge/hostkeys/accept", body); rec.Code != http.StatusBadRequest {
		t.Errorf("POST edge URL with home scope: status = %d, want 400", rec.Code)
	}
}

func TestNoMembershipsMeansUnrestricted(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	seedTargetRun(t, st, "home")
	seedTargetRun(t, st, "edge")
	seedUser(t, st.Users(), "bob", "pw")
	s := newAccessTestServer(st, st.Users(), st.Access(), &recEnqueuer{status: "queued"})
	cookie := loginCookie(t, s, "bob", "pw")

	rec := getAuthWith(t, s, "/api/runs", map[string]string{"Cookie": cookie.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET runs: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var runs []struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode runs: %v", err)
	}
	if len(runs) != 2 {
		t.Errorf("runs = %d, want both collections while no membership is configured", len(runs))
	}
}
