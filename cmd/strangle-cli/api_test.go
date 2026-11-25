package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kakwa/strangle/internal/cfg"
	"github.com/spf13/cobra"
)

type recordedAPIRequest struct {
	method string
	path   string
	body   string
	auth   string
}

func apiTestConfigs(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	serverPath := filepath.Join(dir, "server.yml")
	collectionPath := filepath.Join(dir, "collection.yml")
	if err := os.WriteFile(serverPath, []byte("listen_addr: 127.0.0.1:1\ndatabase:\n  dsn: postgres://postgres@localhost/strangle\nauth:\n  cli_token: st_testtoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(collectionPath, []byte("collections:\n  - name: demo\n    org: lab\n    path: .\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return serverPath, collectionPath
}

func executeAPICommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{
		Use:           "strangle-cli",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.AddCommand(apiCommands()...)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func apiServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *[]recordedAPIRequest) {
	t.Helper()
	requests := &[]recordedAPIRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		*requests = append(*requests, recordedAPIRequest{
			method: r.Method,
			path:   r.URL.RequestURI(),
			body:   string(body),
			auth:   r.Header.Get("Authorization"),
		})
		w.Header().Set("Content-Type", "application/json")
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server, requests
}

func apiFlags(serverPath, collectionPath, base string) []string {
	return []string{
		"--server-config", serverPath,
		"--collection-config", collectionPath,
		"--url", base,
	}
}

func appendArgs(base []string, extra ...string) []string {
	return append(append([]string(nil), base...), extra...)
}

func TestHostsCommandUsesScopedPath(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"name":"web1","ip":"10.0.0.1","last_status":"ok"}]`)
	})

	out, err := executeAPICommand(t, appendArgs(apiFlags(serverPath, collectionPath, server.URL), "hosts", "demo")...)
	if err != nil {
		t.Fatalf("hosts: %v (output %q)", err, out)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(*requests))
	}
	got := (*requests)[0]
	if got.method != http.MethodGet || got.path != "/api/v1/orgs/lab/collections/demo/hosts" {
		t.Errorf("hosts request = %s %s, want GET canonical hosts path", got.method, got.path)
	}
	if got.auth != "Bearer st_testtoken" {
		t.Errorf("Authorization = %q, want the cli token", got.auth)
	}
	if want := "web1\t10.0.0.1\tok\t\t\t\t\n"; out != want {
		t.Errorf("hosts output = %q, want %q", out, want)
	}
}

func TestHostCommandUsesPlaybookScopedRuns(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/orgs/lab/collections/demo/hosts":
			_, _ = io.WriteString(w, `[{"name":"web1","ip":"10.0.0.1","last_status":"ok"}]`)
		case "/api/v1/orgs/lab/collections/demo/playbooks/site/hosts/web1/runs":
			_, _ = io.WriteString(w, `[{"id":7,"playbook":"site","success":true}]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	out, err := executeAPICommand(t, appendArgs(apiFlags(serverPath, collectionPath, server.URL),
		"host", "web1", "demo", "--playbook", "site", "--limit", "5")...)
	if err != nil {
		t.Fatalf("host: %v (output %q)", err, out)
	}
	if len(*requests) != 2 {
		t.Fatalf("requests = %d, want 2 (no changes endpoint)", len(*requests))
	}
	wantPaths := []string{
		"/api/v1/orgs/lab/collections/demo/hosts",
		"/api/v1/orgs/lab/collections/demo/playbooks/site/hosts/web1/runs?limit=5",
	}
	for i, want := range wantPaths {
		if (*requests)[i].method != http.MethodGet {
			t.Errorf("request %d method = %s, want GET", i, (*requests)[i].method)
		}
		if (*requests)[i].path != want {
			t.Errorf("request %d path = %s, want %s", i, (*requests)[i].path, want)
		}
	}
	for _, line := range []string{"host\tweb1\t10.0.0.1\tok\t\t\n", "run\t7\t\tsite\ttrue\t\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("output %q does not contain %q", out, line)
		}
	}
	if strings.Contains(out, "change\t") {
		t.Errorf("output %q still contains changes", out)
	}
}

func TestHostCommandRequiresPlaybook(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	_, err := executeAPICommand(t, appendArgs(apiFlags(serverPath, collectionPath, "http://127.0.0.1:1"),
		"host", "web1", "demo")...)
	if err == nil || !strings.Contains(err.Error(), "playbook") {
		t.Errorf("host without --playbook: err = %v, want a playbook error", err)
	}
}

func TestHostCommandOrgOverrideSkipsCollectionLookup(t *testing.T) {
	serverPath, _ := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/hosts") {
			_, _ = io.WriteString(w, `[{"name":"web1"}]`)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	})

	out, err := executeAPICommand(t, appendArgs(apiFlags(serverPath, filepath.Join(t.TempDir(), "missing.yml"), server.URL),
		"host", "web1", "demo", "--org", "other", "--playbook", "site")...)
	if err != nil {
		t.Fatalf("host with --org: %v (output %q)", err, out)
	}
	if got := (*requests)[0].path; got != "/api/v1/orgs/other/collections/demo/hosts" {
		t.Errorf("first request path = %s, want the --org override", got)
	}
	if got := (*requests)[1].path; got != "/api/v1/orgs/other/collections/demo/playbooks/site/hosts/web1/runs" {
		t.Errorf("second request path = %s, want the --org override", got)
	}
}

func TestRunCommandsAreRemoved(t *testing.T) {
	for _, name := range []string{"run", "runs"} {
		if _, err := executeAPICommand(t, name); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%s: err = %v, want an unknown command error", name, err)
		}
	}
}

func TestHostkeysListUsesPlaybookScopedPath(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/orgs/lab/collections/demo/playbooks/site/hosts/web1/hostkeys" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `[{"target":"demo/site","host":"web1","fingerprint":"SHA256:abc","status":"pending"}]`)
	})
	flags := apiFlags(serverPath, collectionPath, server.URL)

	out, err := executeAPICommand(t, appendArgs(flags, "hostkeys", "list", "demo", "web1", "--playbook", "site")...)
	if err != nil {
		t.Fatalf("hostkeys list: %v (output %q)", err, out)
	}
	if want := "demo/site\tweb1\tSHA256:abc\tpending\t\n"; out != want {
		t.Errorf("list output = %q, want %q", out, want)
	}
	if out, err := executeAPICommand(t, appendArgs(flags, "hostkeys", "list", "demo", "--playbook", "site", "--host", "web1")...); err != nil {
		t.Fatalf("hostkeys list with --host: %v (output %q)", err, out)
	}
	for i, request := range *requests {
		if request.method != http.MethodGet {
			t.Errorf("request %d method = %s, want GET", i, request.method)
		}
		if request.path != "/api/v1/orgs/lab/collections/demo/playbooks/site/hosts/web1/hostkeys" {
			t.Errorf("request %d path = %s", i, request.path)
		}
	}
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func TestHostkeysApproveRejectUseJSONPatch(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/api/v1/orgs/lab/collections/demo/playbooks/site/hosts/web1" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"host":"web1"}`)
	})
	flags := apiFlags(serverPath, collectionPath, server.URL)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"approve", []string{"hostkeys", "approve", "demo/site", "web1", "SHA256:abc"}, "approved\tweb1\n"},
		{"approve flag", []string{"hostkeys", "approve", "demo/site", "web1", "--fingerprint", "SHA256:abc"}, "approved\tweb1\n"},
		{"reject", []string{"hostkeys", "reject", "demo", "web1", "--playbook", "site"}, "rejected\tweb1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := executeAPICommand(t, appendArgs(flags, tc.args...)...)
			if err != nil {
				t.Fatalf("%s: %v (output %q)", tc.name, err, out)
			}
			if out != tc.want {
				t.Errorf("output = %q, want %q", out, tc.want)
			}
		})
	}

	if len(*requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(*requests))
	}
	for i, request := range *requests {
		if request.method != http.MethodPatch || request.path != "/api/v1/orgs/lab/collections/demo/playbooks/site/hosts/web1" {
			t.Errorf("request %d = %s %s", i, request.method, request.path)
		}
	}
	for _, i := range []int{0, 1} {
		var approveOps []patchOp
		if err := json.Unmarshal([]byte((*requests)[i].body), &approveOps); err != nil {
			t.Fatalf("decode approve patch %q: %v", (*requests)[i].body, err)
		}
		if len(approveOps) != 2 ||
			approveOps[0].Op != "replace" || approveOps[0].Path != "/status" || approveOps[0].Value != "approved" ||
			approveOps[1].Op != "replace" || approveOps[1].Path != "/fingerprint" || approveOps[1].Value != "SHA256:abc" {
			t.Errorf("approve patch = %v", approveOps)
		}
	}
	var rejectOps []patchOp
	if err := json.Unmarshal([]byte((*requests)[2].body), &rejectOps); err != nil {
		t.Fatalf("decode reject patch %q: %v", (*requests)[2].body, err)
	}
	if len(rejectOps) != 1 || rejectOps[0].Op != "replace" || rejectOps[0].Path != "/status" || rejectOps[0].Value != "rejected" {
		t.Errorf("reject patch = %v", rejectOps)
	}
}

func TestAPICommandReportsHTTPErrors(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, _ := apiServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	})

	_, err := executeAPICommand(t, appendArgs(apiFlags(serverPath, collectionPath, server.URL), "hosts", "demo")...)
	if err == nil {
		t.Fatal("hosts succeeded, want an API error")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "/api/v1/orgs/lab/collections/demo/hosts") {
		t.Errorf("error = %q, want the API message and scoped path", err)
	}
}

func TestAPIBaseURL(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"", "http://localhost:8080"},
		{":9000", "http://localhost:9000"},
		{"0.0.0.0:9000", "http://localhost:9000"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000"},
	}
	for _, tc := range cases {
		if got := apiBaseURL(configForAddr(tc.addr)); got != tc.want {
			t.Errorf("apiBaseURL(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}

func configForAddr(addr string) cfg.ServerConfig {
	return cfg.ServerConfig{ListenAddr: addr}
}

func TestScopePathEscapesSegments(t *testing.T) {
	got := scopePath(apiScope{org: "my org", collection: "demo"},
		"playbooks", "site", "hosts", "web 1", "runs")
	want := "/api/v1/orgs/my%20org/collections/demo/playbooks/site/hosts/web%201/runs"
	if got != want {
		t.Errorf("scopePath = %q, want %q", got, want)
	}
	if parsed, err := url.Parse(got); err != nil || parsed.Path == "" {
		t.Errorf("scopePath is not a valid URL path: %q (%v)", got, err)
	}
}
