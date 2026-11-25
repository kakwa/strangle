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

	"github.com/kakwa/strangle/internal/config"
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
	if err := os.WriteFile(serverPath, []byte("listen_addr: 127.0.0.1:1\nauth:\n  cli_token: st_testtoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(collectionPath, []byte("targets:\n  - name: demo\n    org: lab\n    path: .\n"), 0o600); err != nil {
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

func TestRunCommandUsesScopedPath(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"queued","run_id":123,"playbook":"site","check":true}`)
	})
	args := appendArgs(apiFlags(serverPath, collectionPath, server.URL), "run", "demo", "site", "--check")

	out, err := executeAPICommand(t, args...)
	if err != nil {
		t.Fatalf("run: %v (output %q)", err, out)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(*requests))
	}
	got := (*requests)[0]
	if got.method != http.MethodPost || got.path != "/api/v1/orgs/lab/collections/demo/run" {
		t.Errorf("request = %s %s, want POST canonical run path", got.method, got.path)
	}
	if got.auth != "Bearer st_testtoken" {
		t.Errorf("Authorization = %q, want the cli token", got.auth)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(got.body), &body); err != nil {
		t.Fatalf("decode body %q: %v", got.body, err)
	}
	if body["target"] != "demo" || body["playbook"] != "site" || body["check"] != true {
		t.Errorf("body = %v, want demo/site/check", body)
	}
	if want := "queued\t123\tlab/demo\tsite\ttrue\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestRunsAndHostsCommandsUseScopedPaths(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/orgs/lab/collections/demo/runs":
			_, _ = io.WriteString(w, `[{"id":9,"target":"demo","playbook":"site","state":"completed"}]`)
		case "/api/v1/orgs/lab/collections/demo/hosts":
			_, _ = io.WriteString(w, `[{"name":"web1","ip":"10.0.0.1","last_status":"ok"}]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	out, err := executeAPICommand(t, appendArgs(apiFlags(serverPath, collectionPath, server.URL),
		"runs", "demo", "--playbook", "site", "--limit", "5", "--json")...)
	if err != nil {
		t.Fatalf("runs: %v (output %q)", err, out)
	}
	if !strings.Contains(out, `"playbook": "site"`) {
		t.Errorf("runs JSON output = %q", out)
	}
	got := (*requests)[0]
	if got.method != http.MethodGet || got.path != "/api/v1/orgs/lab/collections/demo/runs?limit=5&playbook=site" {
		t.Errorf("runs request = %s %s", got.method, got.path)
	}

	out, err = executeAPICommand(t, appendArgs(apiFlags(serverPath, collectionPath, server.URL), "hosts", "demo")...)
	if err != nil {
		t.Fatalf("hosts: %v (output %q)", err, out)
	}
	if want := "web1\t10.0.0.1\tok\t\t\t\t\n"; out != want {
		t.Errorf("hosts output = %q, want %q", out, want)
	}
	got = (*requests)[1]
	if got.path != "/api/v1/orgs/lab/collections/demo/hosts" {
		t.Errorf("hosts request path = %s", got.path)
	}
}

func TestHostCommandCombinesScopedRoutes(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/orgs/lab/collections/demo/hosts":
			_, _ = io.WriteString(w, `[{"name":"web1","ip":"10.0.0.1","last_status":"ok"}]`)
		case "/api/v1/orgs/lab/collections/demo/hosts/web1/runs":
			_, _ = io.WriteString(w, `[{"id":7,"playbook":"site","success":true}]`)
		case "/api/v1/orgs/lab/collections/demo/hosts/web1/changes":
			_, _ = io.WriteString(w, `[{"task":"install","status":"changed"}]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	out, err := executeAPICommand(t, appendArgs(apiFlags(serverPath, collectionPath, server.URL),
		"host", "web1", "demo")...)
	if err != nil {
		t.Fatalf("host: %v (output %q)", err, out)
	}
	if len(*requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(*requests))
	}
	paths := []string{(*requests)[0].path, (*requests)[1].path, (*requests)[2].path}
	want := []string{
		"/api/v1/orgs/lab/collections/demo/hosts",
		"/api/v1/orgs/lab/collections/demo/hosts/web1/runs",
		"/api/v1/orgs/lab/collections/demo/hosts/web1/changes",
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("request %d path = %s, want %s", i, paths[i], want[i])
		}
	}
	for _, line := range []string{"host\tweb1\t10.0.0.1\tok\t\t\n", "run\t7\t\tsite\ttrue\t\n", "change\tinstall\tchanged\t\t\n"} {
		if !strings.Contains(out, line) {
			t.Errorf("output %q does not contain %q", out, line)
		}
	}
}

func TestHostkeysCommandsUseScopedPaths(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/orgs/lab/collections/demo/hostkeys":
			_, _ = io.WriteString(w, `[{"target":"demo/site","host":"web1","fingerprint":"SHA256:abc","status":"pending"}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/hostkeys/accept"):
			_, _ = io.WriteString(w, `{"status":"accepted","host":"web1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/hostkeys/reject"):
			_, _ = io.WriteString(w, `{"status":"rejected","host":"web1"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	flags := apiFlags(serverPath, collectionPath, server.URL)

	if out, err := executeAPICommand(t, appendArgs(flags, "hostkeys", "list", "demo")...); err != nil {
		t.Fatalf("list: %v (output %q)", err, out)
	}
	if out, err := executeAPICommand(t, appendArgs(flags, "hostkeys", "accept", "demo/site", "web1", "SHA256:abc")...); err != nil {
		t.Fatalf("accept: %v (output %q)", err, out)
	}
	if out, err := executeAPICommand(t, appendArgs(flags, "hostkeys", "reject", "demo/site", "web1")...); err != nil {
		t.Fatalf("reject: %v (output %q)", err, out)
	}

	if len(*requests) != 3 {
		t.Fatalf("requests = %d, want 3", len(*requests))
	}
	wantPaths := []string{
		"/api/v1/orgs/lab/collections/demo/hostkeys",
		"/api/v1/orgs/lab/collections/demo/hostkeys/accept",
		"/api/v1/orgs/lab/collections/demo/hostkeys/reject",
	}
	for i, want := range wantPaths {
		if (*requests)[i].path != want {
			t.Errorf("request %d path = %s, want %s", i, (*requests)[i].path, want)
		}
	}
	var acceptBody map[string]any
	if err := json.Unmarshal([]byte((*requests)[1].body), &acceptBody); err != nil {
		t.Fatalf("decode accept body: %v", err)
	}
	if acceptBody["target"] != "demo/site" || acceptBody["host"] != "web1" || acceptBody["fingerprint"] != "SHA256:abc" {
		t.Errorf("accept body = %v", acceptBody)
	}
	var rejectBody map[string]any
	if err := json.Unmarshal([]byte((*requests)[2].body), &rejectBody); err != nil {
		t.Fatalf("decode reject body: %v", err)
	}
	if _, ok := rejectBody["fingerprint"]; ok {
		t.Errorf("reject body has an unexpected fingerprint: %v", rejectBody)
	}
}

func TestAPICommandReportsHTTPErrors(t *testing.T) {
	serverPath, collectionPath := apiTestConfigs(t)
	server, _ := apiServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"boom"}`)
	})

	_, err := executeAPICommand(t, appendArgs(apiFlags(serverPath, collectionPath, server.URL),
		"run", "demo", "site")...)
	if err == nil {
		t.Fatal("run succeeded, want an API error")
	}
	if !strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "/api/v1/orgs/lab/collections/demo/run") {
		t.Errorf("error = %q, want the API message and scoped path", err)
	}
}

func TestAPICommandOrgOverrideSkipsCollectionLookup(t *testing.T) {
	serverPath, _ := apiTestConfigs(t)
	server, requests := apiServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"queued","run_id":1,"playbook":"site"}`)
	})

	out, err := executeAPICommand(t, appendArgs(apiFlags(serverPath, filepath.Join(t.TempDir(), "missing.yml"), server.URL),
		"run", "demo", "site", "--org", "other")...)
	if err != nil {
		t.Fatalf("run with --org: %v (output %q)", err, out)
	}
	if got := (*requests)[0].path; got != "/api/v1/orgs/other/collections/demo/run" {
		t.Errorf("request path = %s, want the --org override", got)
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

func configForAddr(addr string) config.ServerConfig {
	return config.ServerConfig{ListenAddr: addr}
}

func TestScopePathEscapesSegments(t *testing.T) {
	got := scopePath(apiScope{org: "my org", collection: "demo"}, "hosts", "web 1", "runs")
	want := "/api/v1/orgs/my%20org/collections/demo/hosts/web%201/runs"
	if got != want {
		t.Errorf("scopePath = %q, want %q", got, want)
	}
	if parsed, err := url.Parse(got); err != nil || parsed.Path == "" {
		t.Errorf("scopePath is not a valid URL path: %q (%v)", got, err)
	}
}
