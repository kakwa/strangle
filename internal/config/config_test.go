package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeTree writes server.yml, collection.yml and the target's strangle.yml
// under a fresh temp dir and returns the server and collection paths.
func writeTree(t *testing.T, server, collection, strangle string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	serverPath := filepath.Join(dir, "server.yml")
	collectionPath := filepath.Join(dir, "collection.yml")
	targetDir := filepath.Join(dir, "proj")
	writeFile(t, serverPath, server)
	writeFile(t, collectionPath, strings.ReplaceAll(collection, "__TARGET__", targetDir))
	if strangle != "" {
		writeFile(t, filepath.Join(targetDir, "strangle.yml"), strangle)
	}
	return serverPath, collectionPath
}

const (
	collOne = `
targets:
  - name: one
    path: __TARGET__
    git:
      url: https://example.com/one.git
      branch: main
`
	strangleFull = `
playbooks:
  - site.yml
  - dns.yml
inventory: inventory
user: deploy
become: true
become_method: su
become_user: root
become_password_file: /run/secrets/become
connection_password_file: /run/secrets/conn
vault_password_file: /run/secrets/vault
ssh:
  enabled: true
  use_agent: true
`
)

func TestLoad(t *testing.T) {
	serverPath, collectionPath := writeTree(t, `
listen_addr: ":9000"
log_level: debug
interval: 5m
database:
  dsn: postgres://u:p@localhost/db
auth:
  token: abc
`, collOne, strangleFull)

	targetDir := filepath.Join(filepath.Dir(collectionPath), "proj")

	cfg, err := Load(serverPath, collectionPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if got := cfg.Server; got.ListenAddr != ":9000" || got.LogLevel != "debug" ||
		got.Interval != 5*time.Minute || got.Database.DSN != "postgres://u:p@localhost/db" || got.Auth.Token != "abc" {
		t.Errorf("Server = %+v", got)
	}

	if len(cfg.Targets) != 1 {
		t.Fatalf("len(Targets) = %d, want 1", len(cfg.Targets))
	}
	tr := cfg.Targets[0]
	if tr.Name != "one" || tr.Path != targetDir {
		t.Errorf("target identity = %q %q, want one %q", tr.Name, tr.Path, targetDir)
	}
	if tr.Git.URL != "https://example.com/one.git" || tr.Git.Branch != "main" {
		t.Errorf("Git = %+v", tr.Git)
	}
	if tr.WorkingDir != targetDir {
		t.Errorf("WorkingDir = %q, want default %q", tr.WorkingDir, targetDir)
	}
	if !slices.Equal(tr.Playbooks, []string{"site.yml", "dns.yml"}) {
		t.Errorf("Playbooks = %v", tr.Playbooks)
	}
	if tr.Inventory != "inventory" {
		t.Errorf("Inventory = %q", tr.Inventory)
	}
	if !tr.SSH.Enabled || tr.SSH.Port != 22 || tr.SSH.HostKeyPolicy != PolicyAPI {
		t.Errorf("SSH defaults = %+v", tr.SSH)
	}
	if tr.SSH.User != "deploy" {
		t.Errorf("SSH.User = %q, want target-level user %q", tr.SSH.User, "deploy")
	}
	if got, want := tr.SSH.KnownHostsFile, filepath.Join(targetDir, "known_hosts.strangle"); got != want {
		t.Errorf("SSH.KnownHostsFile = %q, want %q", got, want)
	}
	if got, want := tr.SSH.AutoAcceptFile, filepath.Join(targetDir, "known_hosts.auto.json"); got != want {
		t.Errorf("SSH.AutoAcceptFile = %q, want %q", got, want)
	}
	if got, want := tr.SSH.PendingFile, filepath.Join(targetDir, "hostkeys.pending.json"); got != want {
		t.Errorf("SSH.PendingFile = %q, want %q", got, want)
	}

	if got, want := tr.PlaybookPath("site.yml"), filepath.Join(targetDir, "site.yml"); got != want {
		t.Errorf("PlaybookPath(site.yml) = %q, want %q", got, want)
	}
	if got := tr.PlaybookPath("/abs/site.yml"); got != "/abs/site.yml" {
		t.Errorf("PlaybookPath(abs) = %q", got)
	}
	if got, want := tr.InventoryPath(), filepath.Join(targetDir, "inventory"); got != want {
		t.Errorf("InventoryPath() = %q, want %q", got, want)
	}

	wantArgs := []string{
		"--diff", "-i", "inventory",
		"-u", "deploy",
		"-b", "--become-method", "su", "--become-user", "root", "--become-password-file", "/run/secrets/become",
		"--connection-password-file", "/run/secrets/conn",
		"--vault-password-file", "/run/secrets/vault",
		"site.yml",
	}
	if got := tr.Args("site.yml"); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("Args() = %v, want %v", got, wantArgs)
	}
}

func TestLoadDefaults(t *testing.T) {
	serverPath, collectionPath := writeTree(t, "# empty\n", collOne, `
working_dir: playbooks
playbooks:
  - site.yml
inventory: /abs/inventory
`)

	cfg, err := Load(serverPath, collectionPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got := cfg.Server; got.ListenAddr != ":8080" || got.LogLevel != "info" || got.Interval != 15*time.Minute {
		t.Errorf("Server defaults = %+v", got)
	}
	tr := cfg.Targets[0]
	targetDir := filepath.Join(filepath.Dir(collectionPath), "proj")
	if got, want := tr.WorkingDir, filepath.Join(targetDir, "playbooks"); got != want {
		t.Errorf("WorkingDir = %q, want relative resolved to %q", got, want)
	}
	if got, want := tr.InventoryPath(), "/abs/inventory"; got != want {
		t.Errorf("InventoryPath() = %q, want %q", got, want)
	}
	if tr.SSH.Enabled || tr.SSH.Port != 0 || tr.SSH.HostKeyPolicy != "" {
		t.Errorf("SSH should keep zero values when disabled: %+v", tr.SSH)
	}
	if got := tr.Args("site.yml"); !slices.Equal(got, []string{"--diff", "-i", "/abs/inventory", "site.yml"}) {
		t.Errorf("Args() = %v", got)
	}
}

func TestLoadErrors(t *testing.T) {
	const (
		serverOK = `listen_addr: ":9000"`
		collOK   = "targets:\n  - name: one\n    path: __TARGET__\n"
		strOK    = "playbooks: [site.yml]\ninventory: inventory\n"
	)

	cases := []struct {
		name     string
		server   string
		coll     string
		strangle string
		want     string
	}{
		{
			name:     "empty collection",
			server:   serverOK,
			coll:     "targets: []\n",
			strangle: strOK,
			want:     "targets is required",
		},
		{
			name:     "target without name",
			server:   serverOK,
			coll:     "targets:\n  - path: x\n",
			strangle: strOK,
			want:     "needs a name",
		},
		{
			name:     "target without path",
			server:   serverOK,
			coll:     "targets:\n  - name: one\n",
			strangle: strOK,
			want:     "needs a path",
		},
		{
			name:     "duplicate target name",
			server:   serverOK,
			coll:     "targets:\n  - name: one\n    path: a\n  - name: one\n    path: b\n",
			strangle: strOK,
			want:     "duplicate target name",
		},
		{
			name:     "missing strangle.yml",
			server:   serverOK,
			coll:     collOK,
			strangle: "",
			want:     "read",
		},
		{
			name:     "missing playbooks",
			server:   serverOK,
			coll:     collOK,
			strangle: "inventory: inventory\n",
			want:     "playbooks is required",
		},
		{
			name:     "missing inventory",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks: [site.yml]\n",
			want:     "inventory is required",
		},
		{
			name:     "invalid log level",
			server:   "log_level: verbose\n",
			coll:     collOK,
			strangle: strOK,
			want:     "log_level",
		},
		{
			name:     "invalid host key policy",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks: [site.yml]\ninventory: inventory\nssh:\n  enabled: true\n  host_key_policy: nope\n",
			want:     "host_key_policy",
		},
		{
			name:     "ca policy without ca files",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks: [site.yml]\ninventory: inventory\nssh:\n  enabled: true\n  host_key_policy: ca\n",
			want:     "ca requires",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serverPath, collectionPath := writeTree(t, tc.server, tc.coll, tc.strangle)
			_, err := Load(serverPath, collectionPath)
			if err == nil {
				t.Fatalf("Load() error = nil, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load() error = %q, want substring %q", err, tc.want)
			}
		})
	}

	t.Run("missing server file", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "server.yml"), "collection.yml")
		if err == nil {
			t.Fatal("Load() error = nil, want read error")
		}
	})
}

func TestLoadRelativeTargetPath(t *testing.T) {
	// A target path relative to the collection file's directory resolves
	// against that directory, not the working directory.
	serverPath, collectionPath := writeTree(t, "listen_addr: \":9000\"\n", "targets:\n  - name: one\n    path: proj\n", "\nplaybooks:\n  - site.yml\ninventory: inventory\n")

	cfg, err := Load(serverPath, collectionPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got, want := cfg.Targets[0].Path, filepath.Join(filepath.Dir(collectionPath), "proj"); got != want {
		t.Errorf("target path = %q, want %q", got, want)
	}
}

func TestConfigTarget(t *testing.T) {
	cfg := Config{Targets: []TargetConfig{{Name: "a"}, {Name: "b"}}}
	if _, ok := cfg.Target("b"); !ok {
		t.Error("Target(b) = not found, want found")
	}
	if _, ok := cfg.Target("c"); ok {
		t.Error("Target(c) = found, want not found")
	}
}

func TestHostKeyPolicyValid(t *testing.T) {
	valid := []HostKeyPolicy{PolicyKnownHosts, PolicyAPI, PolicyCA, PolicyAuto, PolicyInsecure}
	for _, p := range valid {
		if !p.Valid() {
			t.Errorf("%s.Valid() = false, want true", p)
		}
	}
	if (HostKeyPolicy("nope")).Valid() {
		t.Error("nope.Valid() = true, want false")
	}
	if (HostKeyPolicy("")).Valid() {
		t.Error("empty.Valid() = true, want false")
	}
}
