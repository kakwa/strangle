package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/api"
	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/model"
	"github.com/kakwa/strangle/tests/testutil"
	"github.com/kakwa/strangle/utils/cfg"
	"go.uber.org/zap"
)

// orgTargets has two configured collections: "home" in the default org and
// "edge" in "lab". Several tests check that they cannot leak into each other.
var orgTargets = []cfg.TargetConfig{
	{
		Name: "home",
		Playbooks: []cfg.PlaybookConfig{
			{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
			{Name: "other", Playbook: "other.yml", Inventory: "inventory.ini"},
		},
	},
	{
		Name: "edge",
		Org:  "lab",
		Playbooks: []cfg.PlaybookConfig{
			{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
		},
	},
}

func newOrgServer(st model.Store, opts ...api.Option) *api.Server {
	apr := applier.NewAnsible(orgTargets, stubExecutor{}, st, zap.NewNop())
	return api.New("test", apr, st, nil, nil, nil, nil, nil, "", zap.NewNop(), opts...)
}

// seedRunAt persists one successful run for a collection/playbook/host triple.
func seedRunAt(t *testing.T, st model.Store, target, playbook, host string, started time.Time) {
	t.Helper()
	if err := st.SaveRun(context.Background(), model.RunInput{
		Run: model.RunRecord{
			Target: target, Playbook: playbook, StartedAt: started,
			FinishedAt: started.Add(time.Second), Success: true, Hosts: 1,
		},
		Hosts: []model.HostRunRecord{{Host: host, Status: "ok", Ok: 1}},
	}); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
}

func TestVersionedRoutesAndFlatAliases(t *testing.T) {
	// This test needs no database: it only exercises routing and public docs.
	s := api.New("test", nil, nil, nil, nil, nil, nil, nil, "", zap.NewNop())

	rec := doGet(t, s, "/api/v1/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/health: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var health map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil || health["status"] != "ok" {
		t.Fatalf("health = %q (%v), want status ok", rec.Body.String(), err)
	}

	rec = doGet(t, s, "/api/v1/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/openapi.json: status = %d, body %s", rec.Code, rec.Body.String())
	}

	// Every flat alias of a versioned endpoint, and every removed endpoint,
	// must fall through to the SPA instead of answering as an API route.
	for _, path := range []string{
		"/api/health", "/api/me", "/api/login", "/api/logout",
		"/api/runs", "/api/v1/runs", "/api/run", "/api/v1/run",
		"/api/stats", "/api/v1/stats", "/api/hosts", "/api/v1/hosts",
		"/api/playbooks", "/api/workers", "/api/sync", "/api/v1/sync",
		"/api/hostkeys", "/api/v1/hostkeys", "/api/ssh/stats", "/api/hostkeys/accept",
		"/api/v1/orgs/default/collections/home/runs",
		"/api/v1/orgs/default/collections/home/stats",
		"/api/v1/orgs/default/collections/home/hostkeys",
		"/api/v1/orgs/default/collections/home/ssh/stats",
	} {
		assertAPIGone(t, doGet(t, s, path), "GET "+path)
	}
	for _, path := range []string{
		"/api/run", "/api/v1/run", "/api/sync", "/api/v1/sync",
		"/api/v1/orgs/default/collections/home/run",
		"/api/v1/orgs/default/collections/home/sync",
		"/api/v1/orgs/default/collections/home/hostkeys/accept",
		"/api/v1/orgs/default/collections/home/hostkeys/reject",
	} {
		assertAPIGone(t, postJSON(t, s, path, `{}`, nil), "POST "+path)
	}
	assertAPIGone(t, deleteWith(t, s, "/api/tokens/ci", nil), "DELETE /api/tokens/ci")
}

func TestOpenAPISpecIsVersionedOnly(t *testing.T) {
	s := api.New("test", nil, nil, nil, nil, nil, nil, nil, "", zap.NewNop())
	rec := doGet(t, s, "/api/v1/openapi.json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type = %q, want application/json", ct)
	}
	var doc struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi = %q, want an OpenAPI 3 document", doc.OpenAPI)
	}
	for _, path := range []string{
		"/api/v1/health", "/api/v1/login", "/api/v1/orgs",
		"/api/v1/orgs/{org}/collections/{collection}/hook",
	} {
		if _, ok := doc.Paths[path]; !ok {
			t.Errorf("spec is missing %s", path)
		}
	}
	for _, path := range []string{"/api/health", "/api/runs", "/api/stats"} {
		if _, ok := doc.Paths[path]; ok {
			t.Errorf("spec still advertises removed path %s", path)
		}
	}
}

func TestOrgAndCollectionResources(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	s := newOrgServer(st)

	rec := doGet(t, s, "/api/v1/orgs")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/orgs: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var orgs []orgInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &orgs); err != nil {
		t.Fatalf("decode orgs: %v", err)
	}
	if len(orgs) != 2 || orgs[0].Name != cfg.DefaultOrg || orgs[1].Name != "lab" {
		t.Fatalf("orgs = %+v, want default and lab in order", orgs)
	}
	if len(orgs[0].Collections) != 1 || orgs[0].Collections[0].Name != "home" {
		t.Errorf("default collections = %+v, want home", orgs[0].Collections)
	}
	if len(orgs[1].Collections) != 1 || orgs[1].Collections[0].Name != "edge" {
		t.Errorf("lab collections = %+v, want edge", orgs[1].Collections)
	}

	// Creating an org is idempotent-safe: the second request conflicts.
	rec = postJSON(t, s, "/api/v1/orgs", `{"name":"newco"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST org: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var created orgInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Name != "newco" || len(created.Collections) != 0 {
		t.Fatalf("created org = %+v (%v), want newco with no collections", created, err)
	}
	if rec = postJSON(t, s, "/api/v1/orgs", `{"name":"newco"}`, nil); rec.Code != http.StatusConflict {
		t.Errorf("duplicate POST org: status = %d, want %d", rec.Code, http.StatusConflict)
	}
	for _, body := range []string{`{"name":"bad name"}`, `{}`, `not json`} {
		if rec = postJSON(t, s, "/api/v1/orgs", body, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("POST org %s: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}

	rec = doGet(t, s, "/api/v1/orgs/newco")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET new org: status = %d, body %s", rec.Code, rec.Body.String())
	}
	rec = doGet(t, s, "/api/v1/orgs/newco/collections")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET new org collections: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var colls []collectionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &colls); err != nil || len(colls) != 0 {
		t.Fatalf("new org collections = %s (%v), want empty", rec.Body.String(), err)
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
	wantLinks := map[string]string{
		"hosts":     "/api/v1/orgs/lab/collections/edge/hosts",
		"playbooks": "/api/v1/orgs/lab/collections/edge/playbooks",
		"tokens":    "/api/v1/orgs/lab/collections/edge/tokens",
		"hook":      "/api/v1/orgs/lab/collections/edge/hook",
	}
	if len(coll.Links) != len(wantLinks) {
		t.Fatalf("links = %+v, want exactly %+v", coll.Links, wantLinks)
	}
	for name, want := range wantLinks {
		if coll.Links[name] != want {
			t.Errorf("link %q = %q, want %q", name, coll.Links[name], want)
		}
	}

	for _, path := range []string{
		"/api/v1/orgs/nope",
		"/api/v1/orgs/nope/collections",
		"/api/v1/orgs/nope/collections/edge",
		"/api/v1/orgs/default/collections/edge",
		"/api/v1/orgs/lab/collections/nope",
	} {
		if rec := doGet(t, s, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 (body %s)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestCollectionScopedResources(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	seedRunAt(t, st, "edge", "site", "web1", base)
	seedRunAt(t, st, "home", "site", "web1", base.Add(time.Hour))
	seedRunAt(t, st, "home", "site", "web1", base.Add(2*time.Hour))
	seedRunAt(t, st, "home", "other", "db1", base.Add(3*time.Hour))
	// A newer run in a different collection must not consume the scoped
	// endpoint's limit.
	seedRunAt(t, st, "edge", "site", "web1", base.Add(4*time.Hour))
	s := newOrgServer(st)

	hostNames := func(path string) []string {
		t.Helper()
		rec := doGet(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, body %s", path, rec.Code, rec.Body.String())
		}
		var hosts []model.HostRecord
		if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
			t.Fatalf("decode hosts: %v", err)
		}
		names := make([]string, 0, len(hosts))
		for _, h := range hosts {
			if h.Target != "home" && h.Target != "edge" {
				t.Errorf("host %+v has an unexpected target", h)
			}
			names = append(names, h.Target+"/"+h.Name)
		}
		return names
	}

	homeHosts := hostNames("/api/v1/orgs/default/collections/home/hosts")
	if len(homeHosts) != 2 {
		t.Errorf("home hosts = %v, want web1 and db1", homeHosts)
	}
	edgeHosts := hostNames("/api/v1/orgs/lab/collections/edge/hosts")
	if len(edgeHosts) != 1 || edgeHosts[0] != "edge/web1" {
		t.Errorf("edge hosts = %v, want only edge/web1", edgeHosts)
	}

	rec := doGet(t, s, "/api/v1/orgs/default/collections/home/playbooks")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET playbooks: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var pbs map[string][]playbookInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &pbs); err != nil {
		t.Fatalf("decode playbooks: %v", err)
	}
	if len(pbs) != 1 || len(pbs["home"]) != 2 {
		t.Fatalf("playbooks = %+v, want two home playbooks and no other collection", pbs)
	}
	byName := map[string]playbookInfo{}
	for _, pb := range pbs["home"] {
		byName[pb.Name] = pb
	}
	if byName["site"].LastStatus != "ok" || byName["site"].LastRunAt == nil {
		t.Errorf("site playbook = %+v, want last_status ok with a run time", byName["site"])
	}
	if byName["other"].LastStatus != "ok" {
		t.Errorf("other playbook = %+v, want last_status ok", byName["other"])
	}

	hostRuns := func(path string) []model.HostRunRecord {
		t.Helper()
		rec := doGet(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, body %s", path, rec.Code, rec.Body.String())
		}
		var runs []model.HostRunRecord
		if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
			t.Fatalf("decode host runs: %v", err)
		}
		return runs
	}
	homeRuns := hostRuns("/api/v1/orgs/default/collections/home/playbooks/site/hosts/web1/runs")
	if len(homeRuns) != 2 {
		t.Fatalf("home site/web1 runs = %+v, want two", homeRuns)
	}
	for _, run := range homeRuns {
		if run.Target != "home" || run.Playbook != "site" || run.Host != "web1" {
			t.Errorf("host run = %+v, want home/site/web1", run)
		}
	}
	if got := hostRuns("/api/v1/orgs/default/collections/home/playbooks/site/hosts/web1/runs?limit=1"); len(got) != 1 {
		t.Errorf("limit=1: got %d runs, want 1", len(got))
	}
	if got := hostRuns("/api/v1/orgs/default/collections/home/playbooks/site/hosts/db1/runs"); len(got) != 0 {
		t.Errorf("db1 under site = %+v, want no runs", got)
	}
	edgeRuns := hostRuns("/api/v1/orgs/lab/collections/edge/playbooks/site/hosts/web1/runs")
	if len(edgeRuns) != 2 {
		t.Fatalf("edge site/web1 runs = %+v, want two", edgeRuns)
	}
	for _, run := range edgeRuns {
		if run.Target != "edge" || run.Playbook != "site" || run.Host != "web1" {
			t.Errorf("edge host run = %+v, want edge/site/web1", run)
		}
	}

	for _, path := range []string{
		"/api/v1/orgs/default/collections/home/playbooks/nope/hosts/web1/runs",
		"/api/v1/orgs/default/collections/nope/playbooks/site/hosts/web1/runs",
		"/api/v1/orgs/lab/collections/home/playbooks/site/hosts/web1/runs",
	} {
		if rec := doGet(t, s, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404 (body %s)", path, rec.Code, rec.Body.String())
		}
	}
}
