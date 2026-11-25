package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"go.uber.org/zap"
)

// newOrgTestServer builds a server with two orgs: the default one holding
// "home", and "lab" holding "edge".
func newOrgTestServer(enq RunEnqueuer, st store.Store) *Server {
	targets := []config.TargetConfig{
		{
			Name: "home",
			Playbooks: []config.PlaybookConfig{
				{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
			},
		},
		{
			Name: "edge",
			Org:  "lab",
			Playbooks: []config.PlaybookConfig{
				{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
			},
		},
	}
	apr := applier.NewAnsible(targets, stubExecutor{}, st, zap.NewNop())
	return New("test", apr, st, nil, enq, nil, nil, nil, nil, "", zap.NewNop())
}

func seedRunAt(t *testing.T, st store.Store, target, playbook, host string, started time.Time) {
	t.Helper()
	if err := st.SaveRun(context.Background(), store.RunInput{
		Run: store.RunRecord{
			Target: target, Playbook: playbook, StartedAt: started, FinishedAt: started.Add(time.Second),
			Success: true, Hosts: 1,
		},
		Hosts: []store.HostRunRecord{{Host: host, Status: "ok", Ok: 1}},
	}); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
}

func seedTargetRun(t *testing.T, st store.Store, target string) {
	t.Helper()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	if err := st.SaveRun(context.Background(), store.RunInput{
		Run: store.RunRecord{
			Target: target, Playbook: "site", StartedAt: now, FinishedAt: now,
			Success: true, Hosts: 1,
			Output: "PLAY [all] ***\nfull output for " + target + "\n",
		},
		Hosts: []store.HostRunRecord{{Host: "web1", Status: "ok", Ok: 1}},
	}); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
}

func TestVersionedRoutesAndFlatAliases(t *testing.T) {
	st := store.NewMem()
	seedTargetRun(t, st, "home")
	s := newOrgTestServer(&recEnqueuer{status: "queued"}, st)

	for _, path := range []string{"/api/health", "/api/v1/health", "/api/runs", "/api/v1/runs"} {
		if rec := doGet(t, s, path); rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200 (body %s)", path, rec.Code, rec.Body.String())
		}
	}

	var flat, versioned []store.RunRecord
	if rec := doGet(t, s, "/api/runs"); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &flat) != nil {
		t.Fatal("cannot decode flat runs")
	}
	if rec := doGet(t, s, "/api/v1/runs"); rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &versioned) != nil {
		t.Fatal("cannot decode versioned runs")
	}
	if len(flat) != 1 || len(versioned) != 1 || flat[0].ID != versioned[0].ID {
		t.Errorf("flat runs = %+v, versioned runs = %+v; want the same single run", flat, versioned)
	}
}

func TestOrgAndCollectionResources(t *testing.T) {
	s := newOrgTestServer(&recEnqueuer{status: "queued"}, store.NewMem())

	rec := doGet(t, s, "/api/v1/orgs")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/orgs: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var orgs []orgInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &orgs); err != nil {
		t.Fatalf("decode orgs: %v", err)
	}
	if len(orgs) != 2 || orgs[0].Name != config.DefaultOrg || orgs[1].Name != "lab" {
		t.Fatalf("orgs = %+v, want default and lab in order", orgs)
	}
	if len(orgs[1].Collections) != 1 || orgs[1].Collections[0].Name != "edge" {
		t.Errorf("lab collections = %+v, want edge", orgs[1].Collections)
	}

	rec = doGet(t, s, "/api/v1/orgs/lab/collections/edge")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET collection: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var coll struct {
		Name      string            `json:"name"`
		Org       string            `json:"org"`
		Playbooks []playbookInfo    `json:"playbooks"`
		Links     map[string]string `json:"links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &coll); err != nil {
		t.Fatalf("decode collection: %v", err)
	}
	if coll.Name != "edge" || coll.Org != "lab" || len(coll.Playbooks) != 1 {
		t.Errorf("collection = %+v, want edge in lab with one playbook", coll)
	}
	if coll.Links["runs"] != "/api/v1/orgs/lab/collections/edge/runs" {
		t.Errorf("runs link = %q", coll.Links["runs"])
	}

	for _, path := range []string{
		"/api/v1/orgs/nope",
		"/api/v1/orgs/nope/collections",
		"/api/v1/orgs/nope/collections/edge",
		"/api/v1/orgs/default/collections/edge",
		"/api/v1/orgs/lab/collections/nope/runs",
	} {
		if rec := doGet(t, s, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestCollectionScopedRunsAndRun(t *testing.T) {
	st := store.NewMem()
	seedTargetRun(t, st, "home")
	seedTargetRun(t, st, "edge")
	enq := &recEnqueuer{status: "queued"}
	s := newOrgTestServer(enq, st)

	rec := doGet(t, s, "/api/v1/orgs/lab/collections/edge/runs")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET collection runs: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var runs []store.RunRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode runs: %v", err)
	}
	if len(runs) != 1 || runs[0].Target != "edge" {
		t.Errorf("collection runs = %+v, want only the edge run", runs)
	}

	rec = doGet(t, s, "/api/v1/orgs/lab/collections/edge/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET collection stats: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var stats store.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.Runs.Total != 1 || stats.Hosts.Total != 1 {
		t.Errorf("collection stats = %+v, want one run and one host", stats)
	}

	// On a collection route the target comes from the URL, not the body.
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		s.Routes().ServeHTTP(out, req)
		return out
	}

	rec = post("/api/v1/orgs/lab/collections/edge/run", `{"playbook":"site"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST collection run: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if enq.last() != "edge/site" {
		t.Errorf("enqueued %q, want edge/site", enq.last())
	}

	rec = post("/api/v1/orgs/default/collections/edge/run", `{"playbook":"site"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("POST run with wrong org: status = %d, want 404", rec.Code)
	}
}

func TestRunDetailAndListingWithoutOutput(t *testing.T) {
	st := store.NewMem()
	seedTargetRun(t, st, "home")
	enq := &recEnqueuer{status: "queued"}
	s := newOrgTestServer(enq, st)

	all, err := st.ListRuns(context.Background(), 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListRuns() = %v, %v", all, err)
	}
	id := strconv.FormatUint(all[0].ID, 10)
	wantOutput := "PLAY [all] ***\nfull output for home\n"

	// The polling listing must not ship the full log 50 times over.
	rec := doGet(t, s, "/api/runs")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET runs: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var listed []store.RunRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode runs: %v", err)
	}
	if len(listed) != 1 || listed[0].Output != "" {
		t.Fatalf("listed runs = %+v, want one run without output", listed)
	}

	for _, path := range []string{
		"/api/runs/" + id,
		"/api/v1/runs/" + id,
		"/api/v1/orgs/default/collections/home/runs/" + id,
	} {
		rec = doGet(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, body %s", path, rec.Code, rec.Body.String())
		}
		var detail store.RunRecord
		if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if detail.Output != wantOutput {
			t.Errorf("GET %s output = %q, want %q", path, detail.Output, wantOutput)
		}
	}

	// A run from another collection is not reachable through this path, and
	// bad or unknown IDs get their usual 400/404.
	for _, tc := range []struct {
		path string
		code int
	}{
		{"/api/v1/orgs/lab/collections/edge/runs/" + id, http.StatusNotFound},
		{"/api/runs/999999", http.StatusNotFound},
		{"/api/runs/nope", http.StatusBadRequest},
	} {
		if rec = doGet(t, s, tc.path); rec.Code != tc.code {
			t.Errorf("GET %s: status = %d, want %d (body %s)", tc.path, rec.Code, tc.code, rec.Body.String())
		}
	}
}

func TestCollectionRunFiltersAndPagination(t *testing.T) {
	st := store.NewMem()
	base := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	seedRunAt(t, st, "edge", "site", "web1", base)
	seedRunAt(t, st, "edge", "site", "db1", base.Add(time.Hour))
	seedRunAt(t, st, "edge", "deploy", "web1", base.Add(2*time.Hour))
	seedRunAt(t, st, "home", "site", "web1", base.Add(3*time.Hour))
	s := newOrgTestServer(&recEnqueuer{status: "queued"}, st)
	collectionPath := "/api/v1/orgs/lab/collections/edge/runs"

	ids := func(query string) ([]uint64, int) {
		t.Helper()
		rec := doGet(t, s, collectionPath+query)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s%s: status = %d, body %s", collectionPath, query, rec.Code, rec.Body.String())
		}
		var runs []store.RunRecord
		if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
			t.Fatalf("decode runs: %v", err)
		}
		total, err := strconv.Atoi(rec.Header().Get("X-Total-Count"))
		if err != nil {
			t.Fatalf("X-Total-Count = %q: %v", rec.Header().Get("X-Total-Count"), err)
		}
		out := make([]uint64, 0, len(runs))
		for _, run := range runs {
			if run.Target != "edge" {
				t.Errorf("run %+v leaked from another collection", run)
			}
			out = append(out, run.ID)
		}
		return out, total
	}

	if got, total := ids(""); len(got) != 3 || total != 3 {
		t.Errorf("unfiltered = %v (total %d), want 3 edge runs", got, total)
	}
	if got, total := ids("?playbook=site"); len(got) != 2 || total != 2 {
		t.Errorf("playbook=site = %v (total %d), want 2 runs", got, total)
	}
	if got, total := ids("?host=web1"); len(got) != 2 || total != 2 {
		t.Errorf("host=web1 = %v (total %d), want 2 runs", got, total)
	}
	if got, total := ids("?playbook=site&host=web1"); len(got) != 1 || total != 1 {
		t.Errorf("site/web1 = %v (total %d), want 1 run", got, total)
	}

	from := url.QueryEscape(base.Add(time.Hour).Format(time.RFC3339))
	to := url.QueryEscape(base.Add(2 * time.Hour).Format(time.RFC3339))
	if got, total := ids("?from=" + from + "&to=" + to); len(got) != 1 || total != 1 {
		t.Errorf("time window = %v (total %d), want the 11:00 run only", got, total)
	}

	if got, total := ids("?limit=1&offset=1"); len(got) != 1 || total != 3 {
		t.Errorf("limit/offset = %v (total %d), want one run out of 3", got, total)
	}
	if got, _ := ids("?offset=99"); len(got) != 0 {
		t.Errorf("offset past the end = %v, want no runs", got)
	}

	for _, query := range []string{
		"?from=not-a-time",
		"?to=not-a-time",
		"?from=2026-11-24T12:00:00Z&to=2026-11-24T11:00:00Z",
	} {
		if rec := doGet(t, s, collectionPath+query); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s%s: status = %d, want 400", collectionPath, query, rec.Code)
		}
	}
}
