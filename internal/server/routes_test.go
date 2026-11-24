package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/collection"
	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/internal/worker"
	"go.uber.org/zap"
)

// fakeWorkers is a canned WorkerReporter for the /api/workers tests.
type fakeWorkers struct {
	statuses []worker.WorkerStatus
}

func (f *fakeWorkers) WorkerStatuses(context.Context) []worker.WorkerStatus {
	return f.statuses
}

// testTargets is the configuration used by the route tests: one target with
// two playbooks.
var testTargets = []config.TargetConfig{{
	Name: "home",
	Playbooks: []config.PlaybookConfig{
		{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
		{Name: "other", Playbook: "other.yml", Inventory: "inventory.ini"},
	},
}}

// newTestServerWithStore builds a Server over the given store.
func newTestServerWithStore(enq RunEnqueuer, st store.Store) *Server {
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	return New("test", apr, st, nil, enq, nil, nil, "", zap.NewNop())
}

func doGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

// seedRun persists one successful run for the named playbook with host web1
// in the inventory (and, optionally, a second host that disappears).
func seedRun(t *testing.T, st store.Store, playbook string, success bool) {
	t.Helper()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	err := st.SaveRun(context.Background(), store.RunInput{
		Run: store.RunRecord{
			Target:     "home",
			Playbook:   playbook,
			StartedAt:  now.Add(-time.Second),
			FinishedAt: now,
			DurationMS: 1000,
			ExitCode:   0,
			Success:    success,
			Hosts:      1,
		},
		Hosts:      []store.HostRunRecord{{Host: "web1", Status: "ok", Ok: 3}},
		Inventory:  []store.InventoryHost{{Name: "web1"}, {Name: "db1"}},
		ScopeNames: []string{"web1", "db1"},
	})
	if err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
}

func TestGetHealth(t *testing.T) {
	s := newTestServer(&recEnqueuer{status: "queued"})

	rec := doGet(t, s, "/api/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["status"] != "ok" {
		t.Errorf("status = %q, want ok", out["status"])
	}
}

func TestGetStats(t *testing.T) {
	st := store.NewMem()
	seedRun(t, st, "site", true)
	seedRun(t, st, "other", false)
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var out store.Stats
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Runs.Total != 2 || out.Runs.Succeeded != 1 || out.Runs.Failed != 1 {
		t.Errorf("runs stats = %+v, want 2 total / 1 ok / 1 failed", out.Runs)
	}
	if out.Hosts.Total != 2 || out.Hosts.InInventory != 2 || out.Hosts.Active != 1 || out.Hosts.Disappeared != 1 {
		t.Errorf("hosts stats = %+v, want 2 total / 2 in inventory / 1 active / 1 disappeared", out.Hosts)
	}
	if len(out.PerPlaybook) != 2 {
		t.Errorf("per_playbook = %d entries, want 2", len(out.PerPlaybook))
	}
}

func TestGetRunsWithLimit(t *testing.T) {
	st := store.NewMem()
	seedRun(t, st, "site", true)
	seedRun(t, st, "other", true)
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/runs?limit=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var runs []store.RunRecord
	if err := json.NewDecoder(rec.Body).Decode(&runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("limit=1: got %d runs, want 1", len(runs))
	}
}

func TestGetHosts(t *testing.T) {
	st := store.NewMem()
	seedRun(t, st, "site", true)
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/hosts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var hosts []store.HostRecord
	if err := json.NewDecoder(rec.Body).Decode(&hosts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]store.HostRecord{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts, want 2", len(hosts))
	}
	if web, ok := byName["web1"]; !ok || !web.Active {
		t.Errorf("web1 = %+v, want active", web)
	}
	if db, ok := byName["db1"]; !ok || db.Active {
		t.Errorf("db1 = %+v, want disappeared (not active)", db)
	}
}

func TestGetHostRuns(t *testing.T) {
	st := store.NewMem()
	seedRun(t, st, "site", true)
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/hosts/web1/runs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var runs []store.HostRunRecord
	if err := json.NewDecoder(rec.Body).Decode(&runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 || runs[0].Host != "web1" || runs[0].Playbook != "site" {
		t.Errorf("runs = %+v, want one web1/site record", runs)
	}
}

func TestGetHostChanges(t *testing.T) {
	st := store.NewMem()
	seedRun(t, st, "site", true)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	err := st.SaveRun(context.Background(), store.RunInput{
		Run: store.RunRecord{
			Target: "home", Playbook: "site",
			StartedAt: now.Add(-time.Second), FinishedAt: now,
			DurationMS: 1000, Success: true,
		},
		Tasks: []store.TaskStateRecord{
			{Host: "web1", Task: "install package", Status: "changed", LastDiff: "- old\n+ new"},
		},
	})
	if err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/hosts/web1/changes")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var changes []store.TaskStateRecord
	if err := json.NewDecoder(rec.Body).Decode(&changes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(changes) != 1 || changes[0].Task != "install package" || changes[0].LastDiff != "- old\n+ new" {
		t.Errorf("changes = %+v, want one changed task with its diff", changes)
	}
}

func TestGetPlaybooks(t *testing.T) {
	st := store.NewMem()
	seedRun(t, st, "site", true)
	seedRun(t, st, "other", false)
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/playbooks")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var out map[string][]playbookInfo
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	home, ok := out["home"]
	if !ok || len(home) != 2 {
		t.Fatalf("home playbooks = %v, want 2 entries", out)
	}
	byName := map[string]playbookInfo{}
	for _, p := range home {
		byName[p.Name] = p
	}
	if byName["site"].LastStatus != "ok" || byName["site"].LastRunAt == nil {
		t.Errorf("site = %+v, want last_status ok with a run time", byName["site"])
	}
	if byName["other"].LastStatus != "failed" {
		t.Errorf("other = %+v, want last_status failed", byName["other"])
	}
}

func TestGetHostkeysDisabled(t *testing.T) {
	s := newTestServer(&recEnqueuer{status: "queued"}) // no ssh pool

	rec := doGet(t, s, "/api/hostkeys")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var keys []interface{}
	if err := json.NewDecoder(rec.Body).Decode(&keys); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("hostkeys = %v, want empty", keys)
	}
}

func TestSshStatsDisabled(t *testing.T) {
	s := newTestServer(&recEnqueuer{status: "queued"}) // no ssh pool

	rec := doGet(t, s, "/api/ssh/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var stats map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(stats) != 0 {
		t.Errorf("ssh stats = %v, want empty", stats)
	}
}

func TestHostkeyAcceptDisabled(t *testing.T) {
	s := newTestServer(&recEnqueuer{status: "queued"}) // no ssh pool

	rec := postRunJSON(t, s, "/api/hostkeys/accept", `{"target":"home","host":"web1","fingerprint":"sha256:ab"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHostkeyRejectDisabled(t *testing.T) {
	s := newTestServer(&recEnqueuer{status: "queued"}) // no ssh pool

	rec := postRunJSON(t, s, "/api/hostkeys/reject", `{"target":"home","host":"web1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestSpaFallback(t *testing.T) {
	s := newTestServer(&recEnqueuer{status: "queued"})

	// / and any client-side route fall back to the embedded index.html.
	for _, path := range []string{"/", "/runs/42", "/hosts/web1"} {
		rec := doGet(t, s, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", path, rec.Code, http.StatusOK)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("GET %s: content type = %q, want text/html", path, ct)
		}
	}
}

func TestIntQuery(t *testing.T) {
	cases := []struct {
		q    string
		def  int
		want int
	}{
		{"", 50, 50},
		{"0", 50, 50},
		{"-1", 50, 50},
		{"abc", 50, 50},
		{"7", 50, 7},
	}
	for _, tc := range cases {
		u := "/api/runs"
		if tc.q != "" {
			u = "/api/runs?limit=" + tc.q
		}
		req := httptest.NewRequest(http.MethodGet, u, nil)
		if got := intQuery(req, "limit", tc.def); got != tc.want {
			t.Errorf("intQuery(%q, %d) = %d, want %d", tc.q, tc.def, got, tc.want)
		}
	}
}

func TestGetWorkers(t *testing.T) {
	rep := &fakeWorkers{statuses: []worker.WorkerStatus{
		{ID: 0, PID: 1234, Socket: "/tmp/w0.sock", Alive: true, Jobs: 1, Restarts: 2},
		{ID: 1, PID: 0, Socket: "/tmp/w1.sock", Alive: false},
	}}
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, store.NewMem())
	s.workers = rep

	rec := doGet(t, s, "/api/workers")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var out []worker.WorkerStatus
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("workers = %d entries, want 2", len(out))
	}
	if !out[0].Alive || out[0].PID != 1234 || out[0].Jobs != 1 || out[0].Restarts != 2 {
		t.Errorf("workers[0] = %+v, want alive pid 1234 with 1 job and 2 restarts", out[0])
	}
	if out[1].Alive {
		t.Errorf("workers[1] = %+v, want not alive", out[1])
	}
}

func TestGetWorkersNilReporter(t *testing.T) {
	s := newTestServer(&recEnqueuer{status: "queued"})

	rec := doGet(t, s, "/api/workers")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var out []worker.WorkerStatus
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("workers = %v, want empty", out)
	}
}

func TestListenAndServe(t *testing.T) {
	s := New("127.0.0.1:0",
		applier.NewAnsible(testTargets, stubExecutor{}, store.NewMem(), zap.NewNop()),
		store.NewMem(), nil, &recEnqueuer{status: "queued"}, nil, nil, "", zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- s.ListenAndServe(ctx) }()

	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ListenAndServe() error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ListenAndServe did not return after ctx cancel")
	}
}

// fakeSyncer is a canned Syncer for the /api/sync tests.
type fakeSyncer struct {
	targets []config.TargetConfig
	called  int
	lastTgt string
	lastBr  string
	rev     string
	changed bool
	syncErr error
}

func (f *fakeSyncer) Sync(ctx context.Context, target, branch string) (string, bool, error) {
	f.called++
	f.lastTgt = target
	f.lastBr = branch
	return f.rev, f.changed, f.syncErr
}

func (f *fakeSyncer) Targets() []config.TargetConfig {
	return f.targets
}

// newSyncTestServer builds a Server with the given syncer and auth token.
// It returns the server and the applier so tests can inspect the applier's
// view of the targets after a sync.
func newSyncTestServer(syncer Syncer, token string) (*Server, *applier.Ansible) {
	apr := applier.NewAnsible(testTargets, stubExecutor{}, store.NewMem(), zap.NewNop())
	s := New("test", apr, store.NewMem(), nil, &recEnqueuer{status: "queued"}, nil, syncer, token, zap.NewNop())
	return s, apr
}

func postSync(t *testing.T, s *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sync", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

func TestSyncEndpointSuccess(t *testing.T) {
	// The syncer reports a refreshed playbook list for the target.
	refreshed := []config.TargetConfig{{
		Name: "home",
		Playbooks: []config.PlaybookConfig{
			{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
			{Name: "newjob", Playbook: "newjob.yml", Inventory: "inventory.ini"},
		},
	}}
	fake := &fakeSyncer{targets: refreshed, rev: "abc123", changed: true}
	s, apr := newSyncTestServer(fake, "")

	if got := apr.Playbooks("home"); len(got) != 2 || got[1] != "other" {
		t.Fatalf("initial playbooks = %v, want [site other]", got)
	}

	rec := postSync(t, s, `{"target":"home","branch":"develop"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var out struct {
		Target  string `json:"target"`
		Rev     string `json:"rev"`
		Changed bool   `json:"changed"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Target != "home" || out.Rev != "abc123" || !out.Changed {
		t.Errorf("response = %+v, want home/abc123/true", out)
	}
	if fake.called != 1 || fake.lastTgt != "home" || fake.lastBr != "develop" {
		t.Errorf("syncer called with %q/%q (%d calls), want home/develop (1)", fake.lastTgt, fake.lastBr, fake.called)
	}
	// The applier's view was refreshed from the syncer's targets.
	if got := apr.Playbooks("home"); len(got) != 2 || got[1] != "newjob" {
		t.Errorf("applier playbooks after sync = %v, want [site newjob]", got)
	}
}

func TestSyncEndpointErrors(t *testing.T) {
	// Unknown target.
	fake := &fakeSyncer{syncErr: fmt.Errorf("%w: %q", collection.ErrUnknownTarget, "ghost")}
	s, _ := newSyncTestServer(fake, "")
	rec := postSync(t, s, `{"target":"ghost"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown target: status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	// Target without a git upstream.
	fake.syncErr = fmt.Errorf("%w: %q", collection.ErrNotGit, "local")
	rec = postSync(t, s, `{"target":"local"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("not git: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// A git failure (network, auth, broken strangle.yml).
	fake.syncErr = errors.New("git clone: connection refused")
	rec = postSync(t, s, `{"target":"home"}`, nil)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("git failure: status = %d, want %d", rec.Code, http.StatusBadGateway)
	}

	// Empty target.
	rec = postSync(t, s, `{"branch":"main"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty target: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// Malformed body.
	rec = postSync(t, s, `{`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	// No syncer configured.
	none, _ := newSyncTestServer(nil, "")
	rec = postSync(t, none, `{"target":"home"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("nil syncer: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestSyncEndpointAuth(t *testing.T) {
	fake := &fakeSyncer{targets: testTargets, rev: "abc123"}
	s, _ := newSyncTestServer(fake, "sekrit")

	body := `{"target":"home"}`

	// Missing token.
	rec := postSync(t, s, body, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// Wrong token.
	rec = postSync(t, s, body, map[string]string{"Authorization": "Bearer wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}

	// Correct bearer token.
	rec = postSync(t, s, body, map[string]string{"Authorization": "Bearer sekrit"})
	if rec.Code != http.StatusOK {
		t.Errorf("bearer token: status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Correct webhook-friendly token.
	rec = postSync(t, s, body, map[string]string{"X-Strangle-Token": "sekrit"})
	if rec.Code != http.StatusOK {
		t.Errorf("X-Strangle-Token: status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	if fake.called != 2 {
		t.Errorf("syncer called %d times, want 2", fake.called)
	}
}

func TestBearerTokenOK(t *testing.T) {
	s := New("test",
		applier.NewAnsible(testTargets, stubExecutor{}, store.NewMem(), zap.NewNop()),
		store.NewMem(), nil, &recEnqueuer{status: "queued"}, nil, nil, "sekrit", zap.NewNop())

	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"no headers", nil, false},
		{"empty bearer", map[string]string{"Authorization": "Bearer "}, false},
		{"wrong token", map[string]string{"Authorization": "Bearer wrong"}, false},
		{"right bearer", map[string]string{"Authorization": "Bearer sekrit"}, true},
		{"right webhook header", map[string]string{"X-Strangle-Token": "sekrit"}, true},
		{"webhook header ignored when bearer present", map[string]string{"Authorization": "Bearer wrong", "X-Strangle-Token": "sekrit"}, false},
		{"non-bearer auth scheme", map[string]string{"Authorization": "Basic aWQ9"}, false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/sync", strings.NewReader("{}"))
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		if got := s.bearerTokenOK(req); got != tc.want {
			t.Errorf("%s: bearerTokenOK() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// postRunJSON posts a JSON body to the named endpoint.
func postRunJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}
