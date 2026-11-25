package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kakwa/strangle/internal/server"
	"github.com/kakwa/strangle/internal/testutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/applier"
	"github.com/kakwa/strangle/internal/cfg"
	"github.com/kakwa/strangle/internal/collection"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/sshpool"
	"github.com/kakwa/strangle/internal/worker"
	"go.uber.org/zap"
)

// fakeWorkers is a canned server.WorkerReporter for the /api/workers tests.
type fakeWorkers struct {
	statuses []worker.WorkerStatus
}

func (f *fakeWorkers) WorkerStatuses(context.Context) []worker.WorkerStatus {
	return f.statuses
}

// testTargets is the configuration used by the route tests: one target with
// two playbooks.
var testTargets = []cfg.TargetConfig{{
	Name: "home",
	Playbooks: []cfg.PlaybookConfig{
		{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
		{Name: "other", Playbook: "other.yml", Inventory: "inventory.ini"},
	},
}}

// newTestServerWithStore builds a Server over the given db.
func newTestServerWithStore(enq server.RunEnqueuer, st db.Store, opts ...server.Option) *server.Server {
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	return server.New("test", apr, st, nil, enq, nil, nil, nil, nil, "", zap.NewNop(), opts...)
}

func doGet(t *testing.T, s *server.Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

// seedRun persists one successful run for the named playbook with host web1
// in the inventory (and, optionally, a second host that disappears).
func seedRun(t *testing.T, st db.Store, playbook string, success bool) {
	t.Helper()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	err := st.SaveRun(context.Background(), db.RunInput{
		Run: db.RunRecord{
			Target:     "home",
			Playbook:   playbook,
			StartedAt:  now.Add(-time.Second),
			FinishedAt: now,
			DurationMS: 1000,
			ExitCode:   0,
			Success:    success,
			Hosts:      1,
		},
		Hosts:      []db.HostRunRecord{{Host: "web1", Status: "ok", Ok: 3}},
		Inventory:  []db.InventoryHost{{Name: "web1"}, {Name: "db1"}},
		ScopeNames: []string{"web1", "db1"},
		Facts:      map[string]json.RawMessage{"web1": []byte(`{"ansible_hostname":"web1"}`)},
	})
	if err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
}

func TestGetHealth(t *testing.T) {
	s := newTestServer(t, &recEnqueuer{status: "queued"})

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
	st := testutil.OpenDB(t, "server")
	seedRun(t, st, "site", true)
	seedRun(t, st, "other", false)
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/stats")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var out db.Stats
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
	st := testutil.OpenDB(t, "server")
	seedRun(t, st, "site", true)
	seedRun(t, st, "other", true)
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/runs?limit=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var runs []db.RunRecord
	if err := json.NewDecoder(rec.Body).Decode(&runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 {
		t.Errorf("limit=1: got %d runs, want 1", len(runs))
	}
}

func TestGetHosts(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	seedRun(t, st, "site", true)
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/hosts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var hosts []db.HostRecord
	if err := json.NewDecoder(rec.Body).Decode(&hosts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]db.HostRecord{}
	for _, h := range hosts {
		byName[h.Name] = h
	}
	if len(hosts) != 2 {
		t.Fatalf("got %d hosts, want 2", len(hosts))
	}
	if web, ok := byName["web1"]; !ok || !web.Active {
		t.Errorf("web1 = %+v, want active", web)
	} else if got := string(web.Facts); got != `{"ansible_hostname":"web1"}` {
		t.Errorf("web1.Facts = %s, want the stored facts blob", got)
	}
	if db, ok := byName["db1"]; !ok || db.Active {
		t.Errorf("db1 = %+v, want disappeared (not active)", db)
	}
}

func TestGetHostRuns(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	seedRun(t, st, "site", true)
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, st)

	rec := doGet(t, s, "/api/hosts/web1/runs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var runs []db.HostRunRecord
	if err := json.NewDecoder(rec.Body).Decode(&runs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(runs) != 1 || runs[0].Host != "web1" || runs[0].Playbook != "site" {
		t.Errorf("runs = %+v, want one web1/site record", runs)
	}
}

func TestGetHostChanges(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	seedRun(t, st, "site", true)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	err := st.SaveRun(context.Background(), db.RunInput{
		Run: db.RunRecord{
			Target: "home", Playbook: "site",
			StartedAt: now.Add(-time.Second), FinishedAt: now,
			DurationMS: 1000, Success: true,
		},
		Tasks: []db.TaskStateRecord{
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
	var changes []db.TaskStateRecord
	if err := json.NewDecoder(rec.Body).Decode(&changes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(changes) != 1 || changes[0].Task != "install package" || changes[0].LastDiff != "- old\n+ new" {
		t.Errorf("changes = %+v, want one changed task with its diff", changes)
	}
}

func TestGetPlaybooks(t *testing.T) {
	st := testutil.OpenDB(t, "server")
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
	s := newTestServer(t, &recEnqueuer{status: "queued"}) // no ssh pool

	rec := doGet(t, s, "/api/v1/orgs/default/collections/home/hostkeys")
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
	s := newTestServer(t, &recEnqueuer{status: "queued"}) // no ssh pool

	rec := doGet(t, s, "/api/v1/orgs/default/collections/home/ssh/stats")
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
	s := newTestServer(t, &recEnqueuer{status: "queued"}) // no ssh pool

	rec := postRunJSON(t, s, "/api/v1/orgs/default/collections/home/hostkeys/accept",
		`{"target":"home/site","host":"web1","fingerprint":"sha256:ab"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHostkeyRejectDisabled(t *testing.T) {
	s := newTestServer(t, &recEnqueuer{status: "queued"}) // no ssh pool

	rec := postRunJSON(t, s, "/api/v1/orgs/default/collections/home/hostkeys/reject",
		`{"target":"home/site","host":"web1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

// SSH administration is a collection-scoped feature: there are no master or
// flat routes for it, and every request validates the org/collection pair.
func TestCollectionScopedSSHAdminRoutes(t *testing.T) {
	s := newTestServer(t, &recEnqueuer{status: "queued"}) // no ssh pool

	for _, path := range []string{
		"/api/hostkeys", "/api/v1/hostkeys", "/api/ssh/stats", "/api/v1/ssh/stats",
		"/api/hostkeys/accept", "/api/v1/hostkeys/accept",
		"/api/hostkeys/reject", "/api/v1/hostkeys/reject",
	} {
		rec := doGet(t, s, path)
		// An unregistered API path falls through to the SPA handler, which
		// answers with HTML rather than JSON.
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("GET %s: content type = %q, want the SPA fallback (no master-level API route)", path, ct)
		}
	}
	for _, path := range []string{
		"/api/v1/orgs/default/collections/nope/hostkeys",
		"/api/v1/orgs/lab/collections/home/hostkeys",
		"/api/v1/orgs/default/collections/home/ssh/stats",
	} {
		want := http.StatusOK
		if strings.Contains(path, "/nope/") || strings.Contains(path, "/lab/") {
			want = http.StatusNotFound
		}
		if rec := doGet(t, s, path); rec.Code != want {
			t.Errorf("GET %s: status = %d, want %d", path, rec.Code, want)
		}
	}
	for _, suffix := range []string{
		"/api/v1/orgs/default/collections/home/hostkeys/accept",
		"/api/v1/orgs/default/collections/home/hostkeys/reject",
	} {
		rec := postRunJSON(t, s, suffix, `{"target":"home/site","host":"web1"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s: status = %d, want 400 with SSH disabled", suffix, rec.Code)
		}
	}
}

func TestHostKeyApprovalViaVersionedAPI(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "pending.json"), `{
  "10.0.0.9": {"host":"10.0.0.9","fingerprint":"SHA256:abc","public_key":"ssh-ed25519 AAAA","status":"pending","first_seen":"2026-11-25T00:00:00Z"},
  "10.0.0.10": {"host":"10.0.0.10","fingerprint":"SHA256:def","public_key":"ssh-ed25519 BBBB","status":"pending","first_seen":"2026-11-25T00:00:00Z"}
}`)
	conf := cfg.SSHConfig{
		Enabled:        true,
		Password:       "secret",
		HostKeyPolicy:  cfg.PolicyAPI,
		AutoAcceptFile: filepath.Join(dir, "accepted.json"),
		PendingFile:    filepath.Join(dir, "pending.json"),
	}
	mgr, err := sshpool.New(conf)
	if err != nil {
		t.Fatalf("sshpool.New: %v", err)
	}
	pool, err := sshpool.NewMulti(map[string]*sshpool.Manager{"home/site": mgr})
	if err != nil {
		t.Fatalf("sshpool.NewMulti: %v", err)
	}
	defer pool.Close()

	st := testutil.OpenDB(t, "server")
	defer st.Close()
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	s := server.New("test", apr, st, pool, &recEnqueuer{status: "queued"}, nil, nil, nil, nil, "", zap.NewNop())

	homeKeys := "/api/v1/orgs/default/collections/home/hostkeys"
	var keys []sshpool.HostKey
	rec := doGet(t, s, homeKeys)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET hostkeys: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode hostkeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("hostkeys = %+v, want two keys", keys)
	}
	byHost := map[string]sshpool.HostKey{}
	for _, key := range keys {
		byHost[key.Host] = key
	}
	for host, fingerprint := range map[string]string{"10.0.0.9": "SHA256:abc", "10.0.0.10": "SHA256:def"} {
		key, ok := byHost[host]
		if !ok || key.Target != "home/site" || key.Fingerprint != fingerprint {
			t.Errorf("hostkey for %s = %+v, want scope home/site with fingerprint %s", host, key, fingerprint)
		}
	}

	stats := map[string]sshpool.Stats{}
	rec = doGet(t, s, "/api/v1/orgs/default/collections/home/ssh/stats")
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &stats) != nil {
		t.Fatalf("GET ssh stats: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, ok := stats["home/site"]; !ok {
		t.Errorf("ssh stats = %+v, want a home/site scope", stats)
	}

	// Decisions are taken through the collection URL: a key from another
	// collection cannot be approved by guessing its scope in the body.
	wrong := postRunJSON(t, s, "/api/v1/orgs/default/collections/home/hostkeys/accept",
		`{"target":"edge/site","host":"10.0.0.9","fingerprint":"SHA256:abc"}`)
	if wrong.Code != http.StatusBadRequest {
		t.Errorf("accept with another collection's scope: status = %d, want 400", wrong.Code)
	}
	rec = doGet(t, s, "/api/v1/orgs/default/collections/home/hostkeys")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "10.0.0.9") {
		t.Fatalf("GET home hostkeys: status = %d, body %s", rec.Code, rec.Body.String())
	}
	rec = postRunJSON(t, s, "/api/v1/orgs/default/collections/home/hostkeys/accept",
		`{"target":"home/site","host":"10.0.0.9","fingerprint":"SHA256:abc"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("accept: status = %d, body %s", rec.Code, rec.Body.String())
	}
	rec = postRunJSON(t, s, "/api/v1/orgs/default/collections/home/hostkeys/reject",
		`{"target":"home/site","host":"10.0.0.10"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reject: status = %d, body %s", rec.Code, rec.Body.String())
	}

	rec = doGet(t, s, homeKeys)
	keys = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode hostkeys after decisions: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("hostkeys after accept/reject = %+v, want empty", keys)
	}
	// A wrong fingerprint must not approve a key.
	rec = postRunJSON(t, s, homeKeys+"/accept", `{"target":"home/site","host":"10.0.0.9","fingerprint":"SHA256:wrong"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("accept with wrong fingerprint: status = %d, want 400", rec.Code)
	}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func TestSpaFallback(t *testing.T) {
	s := newTestServer(t, &recEnqueuer{status: "queued"})

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

func TestGetWorkers(t *testing.T) {
	rep := &fakeWorkers{statuses: []worker.WorkerStatus{
		{ID: 0, PID: 1234, Socket: "/tmp/w0.sock", Alive: true, Jobs: 1, Restarts: 2},
		{ID: 1, PID: 0, Socket: "/tmp/w1.sock", Alive: false},
	}}
	s := newTestServerWithStore(&recEnqueuer{status: "queued"}, testutil.OpenDB(t, "server"),
		server.WithWorkerReporter(rep))

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
	s := newTestServer(t, &recEnqueuer{status: "queued"})

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
	st := testutil.OpenDB(t, "server")
	s := server.New("127.0.0.1:0",
		applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop()),
		st, nil, &recEnqueuer{status: "queued"}, nil, nil, nil, nil, "", zap.NewNop())

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

// fakeSyncer is a canned server.Syncer for the /api/sync tests.
type fakeSyncer struct {
	targets []cfg.TargetConfig
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

func (f *fakeSyncer) Targets() []cfg.TargetConfig {
	return f.targets
}

// newSyncTestServer builds a Server with the given syncer and auth token.
// It returns the server and the applier so tests can inspect the applier's
// view of the targets after a sync.
func newSyncTestServer(t *testing.T, syncer server.Syncer, token string) (*server.Server, *applier.Ansible) {
	t.Helper()
	st := testutil.OpenDB(t, "server")
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	s := server.New("test", apr, st, nil, &recEnqueuer{status: "queued"}, nil, syncer, nil, nil, token, zap.NewNop())
	return s, apr
}

func postSync(t *testing.T, s *server.Server, body string, headers map[string]string) *httptest.ResponseRecorder {
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
	refreshed := []cfg.TargetConfig{{
		Name: "home",
		Playbooks: []cfg.PlaybookConfig{
			{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
			{Name: "newjob", Playbook: "newjob.yml", Inventory: "inventory.ini"},
		},
	}}
	fake := &fakeSyncer{targets: refreshed, rev: "abc123", changed: true}
	s, apr := newSyncTestServer(t, fake, "")

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
	s, _ := newSyncTestServer(t, fake, "")
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
	none, _ := newSyncTestServer(t, nil, "")
	rec = postSync(t, none, `{"target":"home"}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("nil syncer: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestSyncEndpointAuth(t *testing.T) {
	fake := &fakeSyncer{targets: testTargets, rev: "abc123"}
	s, _ := newSyncTestServer(t, fake, "sekrit")

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

// postRunJSON posts a JSON body to the named endpoint.
func postRunJSON(t *testing.T, s *server.Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}
