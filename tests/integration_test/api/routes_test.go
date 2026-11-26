package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/api"
	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/controller/applier"
	"github.com/kakwa/strangle/controller/collection"
	"github.com/kakwa/strangle/tests/testutil"
	"github.com/kakwa/strangle/utils/sshpool"
	"go.uber.org/zap"
)

// testTargets is the configuration used by most route tests: one target with
// two playbooks.
var testTargets = []cfg.TargetConfig{{
	Name: "home",
	Playbooks: []cfg.PlaybookConfig{
		{Name: "site", Playbook: "site.yml", Inventory: "inventory.ini"},
		{Name: "other", Playbook: "other.yml", Inventory: "inventory.ini"},
	},
}}

// fakeSyncer is a canned Syncer for the collection hook tests.
type fakeSyncer struct {
	targets []cfg.TargetConfig
	called  int
	lastTgt string
	lastBr  string
	rev     string
	changed bool
	syncErr error
}

func (f *fakeSyncer) Sync(_ context.Context, target, branch string) (string, bool, error) {
	f.called++
	f.lastTgt = target
	f.lastBr = branch
	return f.rev, f.changed, f.syncErr
}

func (f *fakeSyncer) Targets() []cfg.TargetConfig { return f.targets }

func newSyncTestServer(t *testing.T, syncer api.Syncer, token string) (*api.Server, *applier.Ansible) {
	t.Helper()
	st := testutil.OpenDB(t, "server")
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	s := api.New("test", apr, st, nil, nil, syncer, nil, nil, token, zap.NewNop())
	return s, apr
}

func postHook(t *testing.T, s *api.Server, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return postJSON(t, s, path, body, headers)
}

const homeHook = "/api/v1/orgs/default/collections/home/hook"

func TestHookSuccess(t *testing.T) {
	// The syncer reports a refreshed playbook list for the collection.
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

	rec := postHook(t, s, homeHook, `{"branch":"develop"}`, nil)
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
	if got := apr.Playbooks("home"); len(got) != 2 || got[1] != "newjob" {
		t.Errorf("applier playbooks after hook = %v, want [site newjob]", got)
	}
}

func TestHookErrors(t *testing.T) {
	errs := &fakeSyncer{syncErr: fmt.Errorf("%w: %q", collection.ErrUnknownTarget, "ghost")}
	s, _ := newSyncTestServer(t, errs, "")

	for _, tc := range []struct {
		name string
		path string
		body string
		code int
	}{
		{"unknown collection", "/api/v1/orgs/default/collections/nope/hook", `{}`, http.StatusNotFound},
		{"wrong org", "/api/v1/orgs/lab/collections/home/hook", `{}`, http.StatusNotFound},
		{"malformed body", homeHook, `{`, http.StatusBadRequest},
		{"unknown target", homeHook, `{}`, http.StatusNotFound},
	} {
		if rec := postHook(t, s, tc.path, tc.body, nil); rec.Code != tc.code {
			t.Errorf("%s: status = %d, want %d (body %s)", tc.name, rec.Code, tc.code, rec.Body.String())
		}
	}

	errs.syncErr = fmt.Errorf("%w: %q", collection.ErrNotGit, "local")
	if rec := postHook(t, s, homeHook, `{}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("not git: status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	errs.syncErr = errors.New("git clone: connection refused")
	if rec := postHook(t, s, homeHook, `{}`, nil); rec.Code != http.StatusBadGateway {
		t.Errorf("git failure: status = %d, want %d", rec.Code, http.StatusBadGateway)
	}

	// No configured git syncer: the hook route reports that it is unavailable.
	none, _ := newSyncTestServer(t, nil, "")
	if rec := postHook(t, none, homeHook, `{}`, nil); rec.Code != http.StatusNotFound {
		t.Errorf("nil syncer: status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHookStaticTokenWithoutUsers(t *testing.T) {
	fake := &fakeSyncer{targets: testTargets, rev: "abc123"}
	s, _ := newSyncTestServer(t, fake, "sekrit")

	// The configured machine token is required even when there are no users.
	if rec := postHook(t, s, homeHook, `{}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("hook without token = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	for _, h := range []map[string]string{
		{"Authorization": "Bearer sekrit"},
		{"X-Strangle-Token": "sekrit"},
	} {
		rec := postHook(t, s, homeHook, `{}`, h)
		if rec.Code != http.StatusOK {
			t.Errorf("hook with %v: status = %d, want %d (body %s)", h, rec.Code, http.StatusOK, rec.Body.String())
		}
	}
	if rec := postHook(t, s, homeHook, `{}`, map[string]string{"Authorization": "Bearer wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("hook with wrong token = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	if fake.called != 2 {
		t.Errorf("syncer called %d times, want 2", fake.called)
	}
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}

func newSSHServer(t *testing.T, pool *sshpool.Multi) *api.Server {
	t.Helper()
	st := testutil.OpenDB(t, "server")
	apr := applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop())
	return api.New("test", apr, st, pool, nil, nil, nil, nil, "", zap.NewNop())
}

func hostKeyPath(host string) string {
	return "/api/v1/orgs/default/collections/home/playbooks/site/hosts/" + host
}

func TestHostKeysWithoutSSH(t *testing.T) {
	s := newTestServer(t) // no SSH pool

	rec := doGet(t, s, hostKeyPath("web1")+"/hostkeys")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET hostkeys: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var keys []sshpool.HostKey
	if err := json.NewDecoder(rec.Body).Decode(&keys); err != nil || len(keys) != 0 {
		t.Fatalf("hostkeys = %+v (%v), want empty", keys, err)
	}

	patch := `[{"op":"replace","path":"/status","value":"approved"}]`
	if rec := patchJSON(t, s, hostKeyPath("web1"), patch, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with SSH disabled = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestHostKeyApprovalViaNewAPI(t *testing.T) {
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
	s := newSSHServer(t, pool)

	// The hostkeys sub-resource is playbook- and host-scoped.
	rec := doGet(t, s, hostKeyPath("10.0.0.9")+"/hostkeys")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET hostkeys: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var keys []sshpool.HostKey
	if err := json.Unmarshal(rec.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode hostkeys: %v", err)
	}
	if len(keys) != 1 || keys[0].Target != "home/site" || keys[0].Host != "10.0.0.9" || keys[0].Fingerprint != "SHA256:abc" {
		t.Fatalf("hostkeys = %+v, want the pending 10.0.0.9 key of home/site", keys)
	}
	if rec := doGet(t, s, hostKeyPath("other-host")+"/hostkeys"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "[]") {
		t.Errorf("unknown host hostkeys = %s, want an empty list", rec.Body.String())
	}

	// A wrong fingerprint is a conflict, not an approval.
	wrong := `[{"op":"replace","path":"/status","value":"approved"},{"op":"replace","path":"/fingerprint","value":"SHA256:wrong"}]`
	if rec := patchJSON(t, s, hostKeyPath("10.0.0.9"), wrong, nil); rec.Code != http.StatusConflict {
		t.Errorf("PATCH with wrong fingerprint = %d, want %d", rec.Code, http.StatusConflict)
	}
	if rec := doGet(t, s, hostKeyPath("10.0.0.9")+"/hostkeys"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "10.0.0.9") {
		t.Errorf("key must still be pending after a failed approval: %s", rec.Body.String())
	}

	// Approval is a JSON Patch on /status, with an optional fingerprint guard.
	approve := `[{"op":"replace","path":"/status","value":"approved"},{"op":"replace","path":"/fingerprint","value":"SHA256:abc"}]`
	rec = patchJSON(t, s, hostKeyPath("10.0.0.9"), approve, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH approve: status = %d, body %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Status string `json:"status"`
		Host   string `json:"host"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil || out.Status != "approved" || out.Host != "10.0.0.9" {
		t.Errorf("approve response = %+v (%v), want approved/10.0.0.9", out, err)
	}
	if rec := doGet(t, s, hostKeyPath("10.0.0.9")+"/hostkeys"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "[]") {
		t.Errorf("approved key is still pending: %s", rec.Body.String())
	}
	if rec := patchJSON(t, s, hostKeyPath("10.0.0.9"), approve, nil); rec.Code != http.StatusNotFound {
		t.Errorf("PATCH already-approved host = %d, want %d", rec.Code, http.StatusNotFound)
	}

	// Rejection needs only the /status operation.
	reject := `[{"op":"replace","path":"/status","value":"rejected"}]`
	rec = patchJSON(t, s, hostKeyPath("10.0.0.10"), reject, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH reject: status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := doGet(t, s, hostKeyPath("10.0.0.10")+"/hostkeys"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), "[]") {
		t.Errorf("rejected key is still pending: %s", rec.Body.String())
	}
}

func TestHostKeyPatchValidation(t *testing.T) {
	s := newTestServer(t) // no SSH pool; validation happens first
	base := hostKeyPath("web1")
	for _, body := range []string{
		`not json`,
		`[{"op":"remove","path":"/status"}]`,
		`[{"op":"replace","path":"/other","value":"approved"}]`,
		`[{"op":"replace","path":"/status","value":"maybe"}]`,
		`[]`,
	} {
		if rec := patchJSON(t, s, base, body, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s: status = %d, want %d", body, rec.Code, http.StatusBadRequest)
		}
	}

	for _, path := range []string{
		"/api/v1/orgs/default/collections/home/playbooks/nope/hosts/web1",
		"/api/v1/orgs/default/collections/nope/playbooks/site/hosts/web1",
		"/api/v1/orgs/lab/collections/home/playbooks/site/hosts/web1",
	} {
		if rec := patchJSON(t, s, path, `[{"op":"replace","path":"/status","value":"approved"}]`, nil); rec.Code != http.StatusNotFound {
			t.Errorf("PATCH %s: status = %d, want 404", path, rec.Code)
		}
		if rec := doGet(t, s, path+"/hostkeys"); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s/hostkeys: status = %d, want 404", path, rec.Code)
		}
	}
}

func TestSpaFallback(t *testing.T) {
	s := newTestServer(t)

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

func TestListenAndServe(t *testing.T) {
	st := testutil.OpenDB(t, "server")
	s := api.New("127.0.0.1:0",
		applier.NewAnsible(testTargets, stubExecutor{}, st, zap.NewNop()),
		st, nil, nil, nil, nil, nil, "", zap.NewNop())

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
