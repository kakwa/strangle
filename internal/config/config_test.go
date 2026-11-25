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
`
	collGit = `
targets:
  - name: one
    path: __TARGET__
    git:
      url: https://example.com/one.git
      branch: main
      token: abc
`
	strangleMinimal = `
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
`
	strangleFull = `
working_dir: playbooks
options:
  user: admin
  ssh:
    enabled: true
    use_agent: true
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
    options:
      user: deploy
      become: true
      become_method: su
      become_user: root
      become_password_file: /run/secrets/become
      connection_password_file: /run/secrets/conn
      vault_password_file: /run/secrets/vault
      ssh:
        port: 2222
        host_key_policy: auto
  - name: dns
    playbook: dns.yml
    inventory: /abs/inventory
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
	wd := filepath.Join(targetDir, "playbooks")

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
	if tr.Git.IsSet() {
		t.Errorf("Git = %+v, want unset", tr.Git)
	}
	if tr.WorkingDir != wd {
		t.Errorf("WorkingDir = %q, want %q", tr.WorkingDir, wd)
	}
	if len(tr.Playbooks) != 2 {
		t.Fatalf("len(Playbooks) = %d, want 2", len(tr.Playbooks))
	}

	site := tr.Playbooks[0]
	if site.Name != "site" || site.Playbook != "site.yml" || site.Inventory != "inventory" || site.WorkingDir != wd {
		t.Errorf("site identity = %+v", site)
	}
	if site.TargetName != "one" || site.TargetPath != targetDir || site.SSHKey() != "one/site" {
		t.Errorf("site scope = %+v", site)
	}
	if site.Options.User != "deploy" || !site.Options.Become ||
		site.Options.BecomeMethod != "su" || site.Options.BecomeUser != "root" ||
		site.Options.BecomePasswordFile != "/run/secrets/become" ||
		site.Options.ConnectionPasswordFile != "/run/secrets/conn" ||
		site.Options.VaultPasswordFile != "/run/secrets/vault" {
		t.Errorf("site options = %+v", site.Options)
	}
	if !site.Options.SSH.Enabled || !site.Options.SSH.UseAgent ||
		site.Options.SSH.Port != 2222 || site.Options.SSH.HostKeyPolicy != PolicyAuto ||
		site.Options.SSH.User != "deploy" {
		t.Errorf("site SSH = %+v", site.Options.SSH)
	}
	prefix := filepath.Join(targetDir, "strangle.site")
	if site.Options.SSH.AutoAcceptFile != prefix+".auto.json" ||
		site.Options.SSH.PendingFile != prefix+".pending.json" ||
		site.Options.SSH.KnownHostsFile != prefix+".known_hosts" {
		t.Errorf("site SSH files = %+v", site.Options.SSH)
	}

	dns := tr.Playbooks[1]
	if dns.Name != "dns" || dns.Options.User != "admin" || dns.Options.Become {
		t.Errorf("dns inherited options = %+v", dns)
	}
	if !dns.Options.SSH.Enabled || dns.Options.SSH.Port != 22 || dns.Options.SSH.HostKeyPolicy != PolicyAPI {
		t.Errorf("dns SSH defaults = %+v", dns.Options.SSH)
	}
	if dns.Options.SSH.User != "admin" {
		t.Errorf("dns SSH.User = %q, want inherited playbook user", dns.Options.SSH.User)
	}
	if dns.SSHKey() != "one/dns" {
		t.Errorf("dns SSHKey = %q", dns.SSHKey())
	}

	if _, ok := tr.Playbook("site"); !ok {
		t.Error("Playbook(site) = not found")
	}
	if _, ok := tr.Playbook("missing"); ok {
		t.Error("Playbook(missing) = found")
	}

	if got, want := site.PlaybookPath(), filepath.Join(wd, "site.yml"); got != want {
		t.Errorf("PlaybookPath() = %q, want %q", got, want)
	}
	if got, want := site.InventoryPath(), filepath.Join(wd, "inventory"); got != want {
		t.Errorf("InventoryPath() = %q, want %q", got, want)
	}
	if got, want := dns.PlaybookPath(), filepath.Join(wd, "dns.yml"); got != want {
		t.Errorf("dns.PlaybookPath() = %q, want %q", got, want)
	}
	if got := dns.InventoryPath(); got != "/abs/inventory" {
		t.Errorf("dns.InventoryPath() = %q", got)
	}

	wantArgs := []string{
		"--diff", "-i", "inventory",
		"-u", "deploy",
		"-b", "--become-method", "su", "--become-user", "root", "--become-password-file", "/run/secrets/become",
		"--connection-password-file", "/run/secrets/conn",
		"--vault-password-file", "/run/secrets/vault",
		"site.yml",
	}
	if got := site.Args(); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("Args() = %v, want %v", got, wantArgs)
	}
	if got := dns.Args(); !slices.Equal(got, []string{"--diff", "-i", "/abs/inventory", "-u", "admin", "dns.yml"}) {
		t.Errorf("dns.Args() = %v", got)
	}

	wantCheck := append([]string{"--diff", "-i", "inventory", "--check"}, wantArgs[3:]...)
	if got := site.Args(true); !reflect.DeepEqual(got, wantCheck) {
		t.Errorf("Args(true) = %v, want %v", got, wantCheck)
	}
}

func TestLoadRetention(t *testing.T) {
	serverPath, collectionPath := writeTree(t, `
retention:
  interval: 2h
  max_age: 48h
  max_count: 1000
`, `
targets:
  - name: one
    path: __TARGET__
    retention:
      max_count: 7
`, strangleMinimal)
	cfg, err := Load(serverPath, collectionPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Server.Retention.Interval != 2*time.Hour ||
		cfg.Server.Retention.MaxAge != 48*time.Hour ||
		cfg.Server.Retention.MaxCount != 1000 {
		t.Fatalf("server retention = %+v", cfg.Server.Retention)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].Retention.MaxCount != 7 ||
		cfg.Targets[0].Retention.MaxAge != 0 {
		t.Fatalf("target retention = %+v", cfg.Targets)
	}
}

func TestLoadHostCleanup(t *testing.T) {
	serverPath, collectionPath := writeTree(t, `
host_cleanup:
  interval: 3h
  delay: 168h
`, `
targets:
  - name: one
    path: __TARGET__
    host_cleanup:
      delay: 24h
`, strangleMinimal)
	cfg, err := Load(serverPath, collectionPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Server.HostCleanup.Interval != 3*time.Hour || cfg.Server.HostCleanup.Delay != 168*time.Hour {
		t.Fatalf("server host cleanup = %+v", cfg.Server.HostCleanup)
	}
	if len(cfg.Targets) != 1 || cfg.Targets[0].HostCleanup.Delay != 24*time.Hour {
		t.Fatalf("target host cleanup = %+v", cfg.Targets)
	}
}

func TestLoadRejectsNegativeHostCleanup(t *testing.T) {
	serverPath, collectionPath := writeTree(t, `
host_cleanup:
  delay: -1h
`, collOne, strangleMinimal)
	if _, err := Load(serverPath, collectionPath); err == nil ||
		!strings.Contains(err.Error(), "host_cleanup.delay") {
		t.Fatalf("Load() error = %v, want a negative delay error", err)
	}
}

func TestLoadRejectsNegativeRetention(t *testing.T) {
	serverPath, collectionPath := writeTree(t, `
retention:
  max_age: -1h
`, collOne, strangleMinimal)
	if _, err := Load(serverPath, collectionPath); err == nil ||
		!strings.Contains(err.Error(), "retention.max_age") {
		t.Fatalf("Load() error = %v, want a negative max_age error", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	serverPath, collectionPath := writeTree(t, "# empty\n", collOne, `
playbooks:
  - name: site
    playbook: /abs/site.yml
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
	if got, want := tr.WorkingDir, targetDir; got != want {
		t.Errorf("WorkingDir = %q, want default %q", got, want)
	}
	pb := tr.Playbooks[0]
	if pb.Schedule != "" {
		t.Errorf("Schedule = %q, want empty (server interval)", pb.Schedule)
	}
	if pb.Options.SSH.Enabled || pb.Options.Become || pb.Options.User != "" {
		t.Errorf("options should keep zero values: %+v", pb.Options)
	}
	if got, want := pb.PlaybookPath(), "/abs/site.yml"; got != want {
		t.Errorf("PlaybookPath() = %q, want %q", got, want)
	}
	if got, want := pb.InventoryPath(), "/abs/inventory"; got != want {
		t.Errorf("InventoryPath() = %q, want %q", got, want)
	}
	if got := pb.Args(); !slices.Equal(got, []string{"--diff", "-i", "/abs/inventory", "/abs/site.yml"}) {
		t.Errorf("Args() = %v", got)
	}
}

func TestLoadSchedules(t *testing.T) {
	serverPath, collectionPath := writeTree(t, "interval: 10m\n", collOne, `
schedule: "*/5 * * * *"        # target-level default for every job
playbooks:
  - name: cron
    playbook: site.yml
    inventory: inventory
  - name: interval
    playbook: site.yml
    inventory: inventory
    interval: 30s              # duration shorthand overrides the default
  - name: weekly
    playbook: site.yml
    inventory: inventory
    schedule: "0 3 * * 1"
  - name: manual
    playbook: site.yml
    inventory: inventory
    schedule: "@off"
`)
	cfg, err := Load(serverPath, collectionPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	tr := cfg.Targets[0]
	if tr.Schedule != "*/5 * * * *" {
		t.Errorf("target Schedule = %q", tr.Schedule)
	}
	want := map[string]string{
		"cron":     "*/5 * * * *",
		"interval": "30s",
		"weekly":   "0 3 * * 1",
		"manual":   "@off",
	}
	for _, pb := range tr.Playbooks {
		if got := want[pb.Name]; got != pb.Schedule {
			t.Errorf("playbook %q Schedule = %q, want %q", pb.Name, pb.Schedule, got)
		}
	}
}

func TestLoadScheduleErrors(t *testing.T) {
	cases := []struct {
		name     string
		strangle string
		want     string
	}{
		{
			name: "both fields",
			strangle: `schedule: 5m
interval: 5m
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
`,
			want: "set either schedule or interval",
		},
		{
			name: "invalid cron field",
			strangle: `schedule: "60 * * * *"
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
`,
			want: "minute",
		},
		{
			name: "invalid interval",
			strangle: `interval: nope
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
`,
			want: "positive duration",
		},
		{
			name: "invalid playbook schedule",
			strangle: `playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
    schedule: "bogus"
`,
			want: "schedule",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serverPath, collectionPath := writeTree(t, "", collOne, tc.strangle)
			_, err := Load(serverPath, collectionPath)
			if err == nil {
				t.Fatal("Load() error = nil, want a schedule error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load() error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadNotifications(t *testing.T) {
	serverPath, collectionPath := writeTree(t, "", `
targets:
  - name: one
    path: __TARGET__
    notifications:
      - type: webhook
        url: https://hooks.example.com/strangle
        headers:
          Authorization: Bearer secret
        timeout: 2s
      - type: slack
        url: https://hooks.slack.com/services/T/B/X
      - type: email
        from: strangle@example.com
        to:
          - ops@example.com
          - oncall@example.com
        smtp: smtp.example.com:587
        username: strangle
        password_file: smtp.secret
`, strangleMinimal)
	writeFile(t, filepath.Join(filepath.Dir(collectionPath), "smtp.secret"), "s3cr3t\n")

	cfg, err := Load(serverPath, collectionPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	got := cfg.Targets[0].Notifications
	if len(got) != 3 {
		t.Fatalf("len(Notifications) = %d, want 3", len(got))
	}
	if got[0].Type != NotificationWebhook || got[0].URL != "https://hooks.example.com/strangle" ||
		got[0].Headers["Authorization"] != "Bearer secret" || got[0].Timeout != 2*time.Second {
		t.Errorf("webhook = %+v", got[0])
	}
	if got[1].Type != NotificationSlack || got[1].Timeout != 10*time.Second {
		t.Errorf("slack = %+v", got[1])
	}
	if got[2].Type != NotificationEmail || got[2].From != "strangle@example.com" ||
		!slices.Equal(got[2].To, []string{"ops@example.com", "oncall@example.com"}) ||
		got[2].SMTP != "smtp.example.com:587" || got[2].Username != "strangle" ||
		got[2].Password != "s3cr3t" {
		t.Errorf("email = %+v", got[2])
	}
}

func TestLoadNotificationErrors(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want string
	}{
		{"unknown type", `type: pager`, "type must be one of"},
		{"missing url", `type: webhook`, "requires a url"},
		{"relative url", "type: webhook\nurl: /hooks/strangle", "absolute http(s) URL"},
		{"bad header", "type: webhook\nurl: https://example.com/hook\nheaders:\n  Bad:Header: x", "invalid header"},
		{"bad timeout", "type: webhook\nurl: https://example.com/hook\ntimeout: nope", "positive duration"},
		{"email missing to", "type: email\nfrom: a@example.com\nsmtp: example.com:25", "at least one"},
		{"email bad from", "type: email\nfrom: nope\nsmtp: example.com:25\nto: [a@example.com]", "invalid email address"},
		{"email bad smtp", "type: email\nfrom: a@example.com\nto: [b@example.com]\nsmtp: example.com", "host:port"},
		{"both passwords", "type: email\nfrom: a@example.com\nto: [b@example.com]\nsmtp: example.com:25\nusername: a\npassword: x\npassword_file: secret", "not both"},
		{"password without username", "type: email\nfrom: a@example.com\nto: [b@example.com]\nsmtp: example.com:25\npassword: x", "requires a username"},
		{"missing password file", "type: email\nfrom: a@example.com\nto: [b@example.com]\nsmtp: example.com:25\nusername: a\npassword_file: missing", "read password_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			collection := "targets:\n  - name: one\n    path: __TARGET__\n    notifications:\n      - " +
				strings.ReplaceAll(tc.spec, "\n", "\n        ")
			serverPath, collectionPath := writeTree(t, "", collection, strangleMinimal)
			_, err := Load(serverPath, collectionPath)
			if err == nil {
				t.Fatal("Load() error = nil, want a notification error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load() error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadServerSandbox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yml")
	writeFile(t, path, `
listen_addr: ":9090"
sandbox:
  enabled: true
  read_only:
    - /usr/local/share/roles
  writable:
    - /var/cache/strangle
`)
	cfg, err := LoadServer(path)
	if err != nil {
		t.Fatalf("LoadServer() error: %v", err)
	}
	if !cfg.Sandbox.Enabled {
		t.Error("sandbox.enabled = false, want true")
	}
	if got := cfg.Sandbox.ReadOnly; !slices.Equal(got, []string{"/usr/local/share/roles"}) {
		t.Errorf("sandbox.read_only = %v", got)
	}
	if got := cfg.Sandbox.Writable; !slices.Equal(got, []string{"/var/cache/strangle"}) {
		t.Errorf("sandbox.writable = %v", got)
	}
}

func TestLoadGitTarget(t *testing.T) {
	// A git-backed target's strangle.yml lives inside the repository and is
	// loaded by the collection manager once the checkout is in place. Load
	// returns a placeholder (no playbooks) without failing on the missing
	// file, whether or not one happens to exist at the path.
	for _, withFile := range []bool{false, true} {
		name := "without strangle.yml"
		strangle := ""
		if withFile {
			name = "with strangle.yml"
			strangle = strangleMinimal
		}
		t.Run(name, func(t *testing.T) {
			serverPath, collectionPath := writeTree(t, "listen_addr: \":9000\"\n", collGit, strangle)

			cfg, err := Load(serverPath, collectionPath)
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			tr := cfg.Targets[0]
			targetDir := filepath.Join(filepath.Dir(collectionPath), "proj")
			if tr.Path != targetDir {
				t.Errorf("Path = %q, want resolved %q", tr.Path, targetDir)
			}
			if tr.WorkingDir != targetDir {
				t.Errorf("WorkingDir = %q, want %q", tr.WorkingDir, targetDir)
			}
			if len(tr.Playbooks) != 0 {
				t.Errorf("Playbooks = %+v, want empty placeholder", tr.Playbooks)
			}
			if tr.Git.URL != "https://example.com/one.git" || tr.Git.Branch != "main" || tr.Git.Token != "abc" {
				t.Errorf("Git = %+v", tr.Git)
			}
			if got, want := tr.Git.EffectiveRef(), "main"; got != want {
				t.Errorf("EffectiveRef() = %q, want %q", got, want)
			}
		})
	}
}

func TestGitConfig(t *testing.T) {
	if (GitConfig{URL: "u"}).IsSet() != true || (GitConfig{}).IsSet() != false {
		t.Error("IsSet() = wrong result")
	}
	cases := []struct {
		name string
		git  GitConfig
		want string
	}{
		{"empty", GitConfig{}, ""},
		{"branch only", GitConfig{Branch: "dev"}, "dev"},
		{"ref only", GitConfig{Ref: "v1.0"}, "v1.0"},
		{"ref beats branch", GitConfig{Ref: "abc123", Branch: "dev"}, "abc123"},
	}
	for _, tc := range cases {
		if got := tc.git.EffectiveRef(); got != tc.want {
			t.Errorf("%s: EffectiveRef() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLoadTargetTemplating(t *testing.T) {
	dir := t.TempDir()
	spec := TargetSpec{Name: "one", Path: dir}

	t.Run("branch and name variables", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, "strangle.yml"), `
working_dir: work/{{ .Branch }}
playbooks:
  - name: site
    playbook: "{{ .Name }}.yml"
    inventory: inventories/{{ .Branch }}.ini
`)
		tr, err := LoadTarget(spec, dir, map[string]string{"Name": "one", "Branch": "main"})
		if err != nil {
			t.Fatalf("LoadTarget() error: %v", err)
		}
		if got, want := tr.WorkingDir, filepath.Join(dir, "work", "main"); got != want {
			t.Errorf("WorkingDir = %q, want %q", got, want)
		}
		pb := tr.Playbooks[0]
		if pb.Playbook != "one.yml" || pb.Inventory != "inventories/main.ini" {
			t.Errorf("playbook = %q %q, want one.yml inventories/main.ini", pb.Playbook, pb.Inventory)
		}
	})

	t.Run("plain strings pass through", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, "strangle.yml"), strangleMinimal)
		tr, err := LoadTarget(spec, dir, nil)
		if err != nil {
			t.Fatalf("LoadTarget() error: %v", err)
		}
		pb := tr.Playbooks[0]
		if pb.Playbook != "site.yml" || pb.Inventory != "inventory" {
			t.Errorf("playbook = %q %q, want site.yml inventory", pb.Playbook, pb.Inventory)
		}
	})

	t.Run("unknown template variable", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, "strangle.yml"), `
playbooks:
  - name: site
    playbook: site.yml
    inventory: "{{ .Nope }}.ini"
`)
		_, err := LoadTarget(spec, dir, map[string]string{"Branch": "main"})
		if err == nil || !strings.Contains(err.Error(), "inventory") {
			t.Fatalf("LoadTarget() error = %v, want an inventory template error", err)
		}
	})

	t.Run("empty branch renders to empty inventory", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, "strangle.yml"), `
playbooks:
  - name: site
    playbook: site.yml
    inventory: "{{ .Branch }}"
`)
		_, err := LoadTarget(spec, dir, map[string]string{"Branch": ""})
		if err == nil || !strings.Contains(err.Error(), "needs an inventory") {
			t.Fatalf("LoadTarget() error = %v, want empty inventory error", err)
		}
	})
}

func TestLoadErrors(t *testing.T) {
	const (
		serverOK = `listen_addr: ":9000"`
		collOK   = "targets:\n  - name: one\n    path: __TARGET__\n"
		strOK    = "playbooks:\n  - name: site\n    playbook: site.yml\n    inventory: inventory\n"
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
			strangle: "working_dir: playbooks\n",
			want:     "playbooks is required",
		},
		{
			name:     "old flat playbook list",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks:\n  - site.yml\ninventory: inventory\n",
			want:     "cannot unmarshal",
		},
		{
			name:     "old top-level inventory",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks:\n  - name: site\n    playbook: site.yml\ninventory: inventory\n",
			want:     "field inventory not found",
		},
		{
			name:     "unknown option",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks:\n  - name: site\n    playbook: site.yml\n    inventory: inv\n    options:\n      user: bob\n      nope: true\n",
			want:     "field nope not found",
		},
		{
			name:     "playbook without name",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks:\n  - playbook: site.yml\n    inventory: inventory\n",
			want:     "needs a name",
		},
		{
			name:     "invalid playbook name",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks:\n  - name: ../site\n    playbook: site.yml\n    inventory: inventory\n",
			want:     "invalid playbook name",
		},
		{
			name:     "duplicate playbook name",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks:\n  - name: site\n    playbook: site.yml\n    inventory: inventory\n  - name: site\n    playbook: dns.yml\n    inventory: inventory\n",
			want:     "duplicate playbook name",
		},
		{
			name:     "missing playbook file",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks:\n  - name: site\n    inventory: inventory\n",
			want:     "needs a playbook file",
		},
		{
			name:     "missing inventory",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks:\n  - name: site\n    playbook: site.yml\n",
			want:     "needs an inventory",
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
			strangle: "playbooks:\n  - name: site\n    playbook: site.yml\n    inventory: inventory\n    options:\n      ssh:\n        enabled: true\n        host_key_policy: nope\n",
			want:     "host_key_policy",
		},
		{
			name:     "ca policy without ca files",
			server:   serverOK,
			coll:     collOK,
			strangle: "playbooks:\n  - name: site\n    playbook: site.yml\n    inventory: inventory\n    options:\n      ssh:\n        enabled: true\n        host_key_policy: ca\n",
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
	serverPath, collectionPath := writeTree(t, "listen_addr: \":9000\"\n", "targets:\n  - name: one\n    path: proj\n", strangleMinimal)

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
