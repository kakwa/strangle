package cfg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCfg(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cfg Suite")
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeTree writes server.yml, collection.yml and the target's strangle.yml
// under a fresh temp dir and returns the server and collection paths.
func writeTree(server, collection, strangle string) (string, string, error) {
	if !strings.Contains(server, "dsn:") {
		server = "database:\n  dsn: postgres://postgres@localhost/strangle\n" + server
	}
	dir := GinkgoT().TempDir()
	serverPath := filepath.Join(dir, "server.yml")
	collectionPath := filepath.Join(dir, "collection.yml")
	targetDir := filepath.Join(dir, "proj")
	if err := writeFile(serverPath, server); err != nil {
		return "", "", err
	}
	if err := writeFile(collectionPath, strings.ReplaceAll(collection, "__TARGET__", targetDir)); err != nil {
		return "", "", err
	}
	if strangle != "" {
		if err := writeFile(filepath.Join(targetDir, "strangle.yml"), strangle); err != nil {
			return "", "", err
		}
	}
	return serverPath, collectionPath, nil
}

const (
	collOne = `
collections:
  - name: one
    path: __TARGET__
`
	collGit = `
collections:
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

var _ = Describe("TestLoad", func() {
	It("loads a full server, collection and target configuration", func() {
		serverPath, collectionPath, err := writeTree(`
listen_addr: ":9000"
log_level: debug
schedule: "@every 5m"
database:
  dsn: postgres://u:p@localhost/db
auth:
  token: abc
`, collOne, strangleFull)
		Expect(err).NotTo(HaveOccurred())

		targetDir := filepath.Join(filepath.Dir(collectionPath), "proj")
		wd := filepath.Join(targetDir, "playbooks")

		cfg, err := Load(serverPath, collectionPath)
		Expect(err).NotTo(HaveOccurred())

		got := cfg.Server
		Expect(got.ListenAddr).To(Equal(":9000"))
		Expect(got.LogLevel).To(Equal("debug"))
		Expect(got.Schedule).To(Equal("@every 5m"))
		Expect(got.Database.DSN).To(Equal("postgres://u:p@localhost/db"))
		Expect(got.Auth.Token).To(Equal("abc"))

		Expect(cfg.Targets).To(HaveLen(1))
		tr := cfg.Targets[0]
		Expect(tr.Name).To(Equal("one"))
		Expect(tr.Path).To(Equal(targetDir))
		Expect(tr.Git.IsSet()).To(BeFalse())
		Expect(tr.WorkingDir).To(Equal(wd))
		Expect(tr.Playbooks).To(HaveLen(2))

		site := tr.Playbooks[0]
		Expect(site.Name).To(Equal("site"))
		Expect(site.Playbook).To(Equal("site.yml"))
		Expect(site.Inventory).To(Equal("inventory"))
		Expect(site.WorkingDir).To(Equal(wd))
		Expect(site.TargetName).To(Equal("one"))
		Expect(site.TargetPath).To(Equal(targetDir))
		Expect(site.SSHKey()).To(Equal("one/site"))
		Expect(site.Options.User).To(Equal("deploy"))
		Expect(site.Options.Become).To(BeTrue())
		Expect(site.Options.BecomeMethod).To(Equal("su"))
		Expect(site.Options.BecomeUser).To(Equal("root"))
		Expect(site.Options.BecomePasswordFile).To(Equal("/run/secrets/become"))
		Expect(site.Options.ConnectionPasswordFile).To(Equal("/run/secrets/conn"))
		Expect(site.Options.VaultPasswordFile).To(Equal("/run/secrets/vault"))
		Expect(site.Options.SSH.Enabled).To(BeTrue())
		Expect(site.Options.SSH.UseAgent).To(BeTrue())
		Expect(site.Options.SSH.Port).To(Equal(2222))
		Expect(site.Options.SSH.HostKeyPolicy).To(Equal(PolicyAuto))
		Expect(site.Options.SSH.User).To(Equal("deploy"))
		prefix := filepath.Join(targetDir, "strangle.site")
		Expect(site.Options.SSH.AutoAcceptFile).To(Equal(prefix + ".auto.json"))
		Expect(site.Options.SSH.PendingFile).To(Equal(prefix + ".pending.json"))
		Expect(site.Options.SSH.KnownHostsFile).To(Equal(prefix + ".known_hosts"))

		dns := tr.Playbooks[1]
		Expect(dns.Name).To(Equal("dns"))
		Expect(dns.Options.User).To(Equal("admin"))
		Expect(dns.Options.Become).To(BeFalse())
		Expect(dns.Options.SSH.Enabled).To(BeTrue())
		Expect(dns.Options.SSH.Port).To(Equal(22))
		Expect(dns.Options.SSH.HostKeyPolicy).To(Equal(PolicyAPI))
		Expect(dns.Options.SSH.User).To(Equal("admin"))
		Expect(dns.SSHKey()).To(Equal("one/dns"))

		_, ok := tr.Playbook("site")
		Expect(ok).To(BeTrue())
		_, ok = tr.Playbook("missing")
		Expect(ok).To(BeFalse())

		Expect(site.PlaybookPath()).To(Equal(filepath.Join(wd, "site.yml")))
		Expect(site.InventoryPath()).To(Equal(filepath.Join(wd, "inventory")))
		Expect(dns.PlaybookPath()).To(Equal(filepath.Join(wd, "dns.yml")))
		Expect(dns.InventoryPath()).To(Equal("/abs/inventory"))

		wantArgs := []string{
			"--diff", "-i", "inventory",
			"-u", "deploy",
			"-b", "--become-method", "su", "--become-user", "root", "--become-password-file", "/run/secrets/become",
			"--connection-password-file", "/run/secrets/conn",
			"--vault-password-file", "/run/secrets/vault",
			"site.yml",
		}
		Expect(site.Args()).To(Equal(wantArgs))
		Expect(dns.Args()).To(Equal([]string{"--diff", "-i", "/abs/inventory", "-u", "admin", "dns.yml"}))

		wantCheck := append([]string{"--diff", "-i", "inventory", "--check"}, wantArgs[3:]...)
		Expect(site.Args(true)).To(Equal(wantCheck))
	})
})

var _ = Describe("TestLoadRetention", func() {
	It("applies server and per-collection retention settings", func() {
		serverPath, collectionPath, err := writeTree(`
retention:
  interval: 2h
  max_age: 48h
  max_count: 1000
`, `
collections:
  - name: one
    path: __TARGET__
    retention:
      max_count: 7
`, strangleMinimal)
		Expect(err).NotTo(HaveOccurred())
		cfg, err := Load(serverPath, collectionPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Server.Retention.Interval).To(Equal(2 * time.Hour))
		Expect(cfg.Server.Retention.MaxAge).To(Equal(48 * time.Hour))
		Expect(cfg.Server.Retention.MaxCount).To(Equal(1000))
		Expect(cfg.Targets).To(HaveLen(1))
		Expect(cfg.Targets[0].Retention.MaxCount).To(Equal(7))
		Expect(cfg.Targets[0].Retention.MaxAge).To(Equal(time.Duration(0)))
	})
})

var _ = Describe("TestLoadTLS", func() {
	It("loads the TLS certificate and key files", func() {
		serverPath, collectionPath, err := writeTree(`
tls:
  cert_file: /etc/strangle/tls.crt
  key_file: /etc/strangle/tls.key
`, collOne, strangleMinimal)
		Expect(err).NotTo(HaveOccurred())
		cfg, err := Load(serverPath, collectionPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Server.TLS.CertFile).To(Equal("/etc/strangle/tls.crt"))
		Expect(cfg.Server.TLS.KeyFile).To(Equal("/etc/strangle/tls.key"))
	})
})

var _ = Describe("TestLoadRejectsPartialTLS", func() {
	It("rejects a tls block with only cert_file", func() {
		serverPath, collectionPath, err := writeTree(`
tls:
  cert_file: /etc/strangle/tls.crt
`, collOne, strangleMinimal)
		Expect(err).NotTo(HaveOccurred())
		_, err = Load(serverPath, collectionPath)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("tls.cert_file"))
	})
})

var _ = Describe("TestLoadHostCleanup", func() {
	It("applies server and per-collection host cleanup settings", func() {
		serverPath, collectionPath, err := writeTree(`
host_cleanup:
  interval: 3h
  delay: 168h
`, `
collections:
  - name: one
    path: __TARGET__
    host_cleanup:
      delay: 24h
`, strangleMinimal)
		Expect(err).NotTo(HaveOccurred())
		cfg, err := Load(serverPath, collectionPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Server.HostCleanup.Interval).To(Equal(3 * time.Hour))
		Expect(cfg.Server.HostCleanup.Delay).To(Equal(168 * time.Hour))
		Expect(cfg.Targets).To(HaveLen(1))
		Expect(cfg.Targets[0].HostCleanup.Delay).To(Equal(24 * time.Hour))
	})
})

var _ = Describe("TestLoadRejectsNegativeHostCleanup", func() {
	It("rejects a negative host cleanup delay", func() {
		serverPath, collectionPath, err := writeTree(`
host_cleanup:
  delay: -1h
`, collOne, strangleMinimal)
		Expect(err).NotTo(HaveOccurred())
		_, err = Load(serverPath, collectionPath)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("host_cleanup.delay"))
	})
})

var _ = Describe("TestLoadRejectsNegativeRetention", func() {
	It("rejects a negative retention max age", func() {
		serverPath, collectionPath, err := writeTree(`
retention:
  max_age: -1h
`, collOne, strangleMinimal)
		Expect(err).NotTo(HaveOccurred())
		_, err = Load(serverPath, collectionPath)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("retention.max_age"))
	})
})

var _ = Describe("TestLoadDefaults", func() {
	It("fills in the default server, target and playbook values", func() {
		serverPath, collectionPath, err := writeTree("# empty\n", collOne, `
playbooks:
  - name: site
    playbook: /abs/site.yml
    inventory: /abs/inventory
`)
		Expect(err).NotTo(HaveOccurred())

		cfg, err := Load(serverPath, collectionPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Server.ListenAddr).To(Equal(":8080"))
		Expect(cfg.Server.LogLevel).To(Equal("info"))
		Expect(cfg.Server.Schedule).To(Equal("@every 15m"))
		targetDir := filepath.Join(filepath.Dir(collectionPath), "proj")
		Expect(cfg.Targets[0].WorkingDir).To(Equal(targetDir))
		pb := cfg.Targets[0].Playbooks[0]
		Expect(pb.Schedule).To(BeEmpty())
		Expect(pb.Options.SSH.Enabled).To(BeFalse())
		Expect(pb.Options.Become).To(BeFalse())
		Expect(pb.Options.User).To(BeEmpty())
		Expect(pb.PlaybookPath()).To(Equal("/abs/site.yml"))
		Expect(pb.InventoryPath()).To(Equal("/abs/inventory"))
		Expect(pb.Args()).To(Equal([]string{"--diff", "-i", "/abs/inventory", "/abs/site.yml"}))
	})
})

var _ = Describe("TestLoadSchedules", func() {
	It("resolves target-level and per-playbook schedules", func() {
		serverPath, collectionPath, err := writeTree("schedule: \"@every 10m\"\n", collOne, `
schedule: "*/5 * * * *"        # target-level default for every job
playbooks:
  - name: cron
    playbook: site.yml
    inventory: inventory
  - name: every
    playbook: site.yml
    inventory: inventory
    schedule: "@every 30s"     # robfig descriptor overrides the default
  - name: weekly
    playbook: site.yml
    inventory: inventory
    schedule: "0 3 * * 1"
  - name: inherits
    playbook: site.yml
    inventory: inventory
`)
		Expect(err).NotTo(HaveOccurred())
		cfg, err := Load(serverPath, collectionPath)
		Expect(err).NotTo(HaveOccurred())
		tr := cfg.Targets[0]
		Expect(tr.Schedule).To(Equal("*/5 * * * *"))
		want := map[string]string{
			"cron":     "*/5 * * * *",
			"every":    "@every 30s",
			"weekly":   "0 3 * * 1",
			"inherits": "*/5 * * * *",
		}
		for _, pb := range tr.Playbooks {
			Expect(pb.Schedule).To(Equal(want[pb.Name]))
		}
	})
})

var _ = Describe("TestLoadScheduleErrors", func() {
	cases := []struct {
		name     string
		strangle string
		want     string
	}{
		{
			name: "invalid cron field",
			strangle: `schedule: "60 * * * *"
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
`,
			want: "invalid schedule",
		},
		{
			name: "legacy interval key",
			strangle: `interval: 5m
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
`,
			want: "field interval not found",
		},
		{
			name: "invalid playbook schedule",
			strangle: `playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
    schedule: "@off"
`,
			want: "invalid schedule",
		},
	}
	for _, tc := range cases {
		It(tc.name, func() {
			serverPath, collectionPath, err := writeTree("", collOne, tc.strangle)
			Expect(err).NotTo(HaveOccurred())
			_, err = Load(serverPath, collectionPath)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(tc.want))
		})
	}
})

var _ = Describe("TestLoadTargetTemplating", func() {
	It("substitutes branch and name variables", func() {
		dir := GinkgoT().TempDir()
		spec := TargetSpec{Name: "one", Path: dir}

		Expect(writeFile(filepath.Join(dir, "strangle.yml"), `
working_dir: work/{{ .Branch }}
playbooks:
  - name: site
    playbook: "{{ .Name }}.yml"
    inventory: inventories/{{ .Branch }}.ini
`)).To(Succeed())
		tr, err := LoadTarget(spec, dir, map[string]string{"Name": "one", "Branch": "main"})
		Expect(err).NotTo(HaveOccurred())
		Expect(tr.WorkingDir).To(Equal(filepath.Join(dir, "work", "main")))
		pb := tr.Playbooks[0]
		Expect(pb.Playbook).To(Equal("one.yml"))
		Expect(pb.Inventory).To(Equal("inventories/main.ini"))
	})

	It("passes plain strings through", func() {
		dir := GinkgoT().TempDir()
		spec := TargetSpec{Name: "one", Path: dir}

		Expect(writeFile(filepath.Join(dir, "strangle.yml"), strangleMinimal)).To(Succeed())
		tr, err := LoadTarget(spec, dir, nil)
		Expect(err).NotTo(HaveOccurred())
		pb := tr.Playbooks[0]
		Expect(pb.Playbook).To(Equal("site.yml"))
		Expect(pb.Inventory).To(Equal("inventory"))
	})

	It("rejects an unknown template variable", func() {
		dir := GinkgoT().TempDir()
		spec := TargetSpec{Name: "one", Path: dir}

		Expect(writeFile(filepath.Join(dir, "strangle.yml"), `
playbooks:
  - name: site
    playbook: site.yml
    inventory: "{{ .Nope }}.ini"
`)).To(Succeed())
		_, err := LoadTarget(spec, dir, map[string]string{"Branch": "main"})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("inventory"))
	})

	It("rejects a branch that renders to an empty inventory", func() {
		dir := GinkgoT().TempDir()
		spec := TargetSpec{Name: "one", Path: dir}

		Expect(writeFile(filepath.Join(dir, "strangle.yml"), `
playbooks:
  - name: site
    playbook: site.yml
    inventory: "{{ .Branch }}"
`)).To(Succeed())
		_, err := LoadTarget(spec, dir, map[string]string{"Branch": ""})
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("needs an inventory"))
	})
})

var _ = Describe("TestLoadErrors", func() {
	const (
		serverOK = `listen_addr: ":9000"`
		collOK   = "collections:\n  - name: one\n    path: __TARGET__\n"
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
			coll:     "collections: []\n",
			strangle: strOK,
			want:     "collections is required",
		},
		{
			name:     "target without name",
			server:   serverOK,
			coll:     "collections:\n  - path: x\n",
			strangle: strOK,
			want:     "needs a name",
		},
		{
			name:     "target without path",
			server:   serverOK,
			coll:     "collections:\n  - name: one\n",
			strangle: strOK,
			want:     "needs a path",
		},
		{
			name:     "duplicate target name",
			server:   serverOK,
			coll:     "collections:\n  - name: one\n    path: a\n  - name: one\n    path: b\n",
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
		It(tc.name, func() {
			serverPath, collectionPath, err := writeTree(tc.server, tc.coll, tc.strangle)
			Expect(err).NotTo(HaveOccurred())
			_, err = Load(serverPath, collectionPath)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(tc.want))
		})
	}

	It("missing server file", func() {
		_, err := Load(filepath.Join(GinkgoT().TempDir(), "server.yml"), "collection.yml")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("TestLoadRelativeTargetPath", func() {
	// A target path relative to the collection file's directory resolves
	// against that directory, not the working directory.
	It("resolves a relative target path against the collection directory", func() {
		serverPath, collectionPath, err := writeTree("listen_addr: \":9000\"\n", "collections:\n  - name: one\n    path: proj\n", strangleMinimal)
		Expect(err).NotTo(HaveOccurred())

		cfg, err := Load(serverPath, collectionPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Targets[0].Path).To(Equal(filepath.Join(filepath.Dir(collectionPath), "proj")))
	})
})

var _ = Describe("TestConfigTarget", func() {
	It("finds targets by name", func() {
		cfg := Config{Targets: []TargetConfig{{Name: "a"}, {Name: "b"}}}
		_, ok := cfg.Target("b")
		Expect(ok).To(BeTrue())
		_, ok = cfg.Target("c")
		Expect(ok).To(BeFalse())
	})
})

var _ = Describe("TestLoadServerCLIToken", func() {
	It("loads both auth tokens from server.yml", func() {
		serverPath, _, err := writeTree(
			"auth:\n  token: sync-secret\n  cli_token: st_cli\n", "collections:\n  - name: one\n    path: __TARGET__\n", strangleMinimal)
		Expect(err).NotTo(HaveOccurred())
		cfg, err := LoadServer(serverPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(cfg.Auth.Token).To(Equal("sync-secret"))
		Expect(cfg.Auth.CLIToken).To(Equal("st_cli"))
	})
})

var _ = Describe("TestLoadCollectionAppliesOrgDefault", func() {
	It("defaults the org to default and keeps explicit ones", func() {
		_, collectionPath, err := writeTree("listen_addr: \":9000\"\n", `
collections:
  - name: one
    path: __TARGET__
  - name: two
    org: lab
    path: __TARGET__
`, strangleMinimal)
		Expect(err).NotTo(HaveOccurred())
		specs, err := LoadCollection(collectionPath)
		Expect(err).NotTo(HaveOccurred())
		Expect(specs).To(HaveLen(2))
		Expect(specs[0].Org).To(Equal(DefaultOrg))
		Expect(specs[1].Org).To(Equal("lab"))
	})
})

var _ = Describe("TestHostKeyPolicyValid", func() {
	It("accepts the known policies and rejects others", func() {
		valid := []HostKeyPolicy{PolicyKnownHosts, PolicyAPI, PolicyCA, PolicyAuto, PolicyInsecure}
		for _, p := range valid {
			Expect(p.Valid()).To(BeTrue())
		}
		Expect((HostKeyPolicy("nope")).Valid()).To(BeFalse())
		Expect((HostKeyPolicy("")).Valid()).To(BeFalse())
	})
})

const strangleInternalCert = `
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
    options:
      ssh:
        enabled: true
        private_key_file: id_ed25519
        host_key_policy: auto
        certificate:
          backend: internal
          principals: [deploy, ubuntu]
          internal:
            ca_key_file: /etc/strangle/ssh_ca
`

var _ = Describe("TestLoadInternalCertificateConfig", func() {
	It("loads an internal CA certificate configuration with defaults", func() {
		serverPath, collectionPath, err := writeTree("listen_addr: \":9000\"\n", collOne, strangleInternalCert)
		Expect(err).NotTo(HaveOccurred())
		cfg, err := Load(serverPath, collectionPath)
		Expect(err).NotTo(HaveOccurred())
		target, ok := cfg.Target("one")
		Expect(ok).To(BeTrue())
		Expect(target.Playbooks).To(HaveLen(1))
		cert := target.Playbooks[0].Options.SSH.Certificate
		Expect(cert).NotTo(BeNil())
		Expect(cert.Backend).To(Equal(CertificateInternal))
		Expect(cert.TTL).To(Equal(10 * time.Minute))
		Expect(cert.KeyIDPrefix).To(Equal("strangle"))
		Expect(cert.Internal.CAKeyFile).To(Equal("/etc/strangle/ssh_ca"))
		Expect(cert.Principals).To(ConsistOf("deploy", "ubuntu"))
	})
})

var _ = Describe("TestLoadCertificateMergeAndVaultDefaults", func() {
	It("merges target-level and playbook certificate options with vault defaults", func() {
		strangle := `
options:
  ssh:
    enabled: true
    private_key_file: id_ed25519
    host_key_policy: auto
    certificate:
      backend: vault
      ttl: 15m
      key_id_prefix: lab
      vault:
        addr: https://vault.example.com
        role: strangle
        role_id_file: /run/secrets/role_id
        secret_id_file: /run/secrets/secret_id
        ca_cert_file: /etc/ssl/vault-ca.pem
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
    options:
      ssh:
        certificate:
          ttl: 30m
          principals: [deploy]
          vault:
            mount: ssh-lab
`
		serverPath, collectionPath, err := writeTree("listen_addr: \":9000\"\n", collOne, strangle)
		Expect(err).NotTo(HaveOccurred())
		cfg, err := Load(serverPath, collectionPath)
		Expect(err).NotTo(HaveOccurred())
		target, _ := cfg.Target("one")
		cert := target.Playbooks[0].Options.SSH.Certificate
		Expect(cert).NotTo(BeNil())
		Expect(cert.TTL).To(Equal(30 * time.Minute))
		Expect(cert.KeyIDPrefix).To(Equal("lab"))
		Expect(cert.Principals).To(ConsistOf("deploy"))
		Expect(cert.Vault.Mount).To(Equal("ssh-lab"))
		Expect(cert.Vault.AuthMount).To(Equal("approle"))
		Expect(cert.Vault.RoleIDFile).To(Equal("/run/secrets/role_id"))
		Expect(cert.Vault.SecretIDFile).To(Equal("/run/secrets/secret_id"))
	})
})

var _ = Describe("TestLoadCertificateValidation", func() {
	cases := []struct {
		name     string
		ssh      string
		contains string
	}{
		{
			name: "unknown backend",
			ssh: `private_key_file: id
        certificate:
          backend: other`,
			contains: "backend must be",
		},
		{
			name: "missing private key",
			ssh: `certificate:
          backend: internal
          internal:
            ca_key_file: /etc/ca`,
			contains: "requires ssh.private_key_file",
		},
		{
			name: "agent not supported",
			ssh: `private_key_file: id
        use_agent: true
        certificate:
          backend: internal
          internal:
            ca_key_file: /etc/ca`,
			contains: "does not support ssh.use_agent",
		},
		{
			name: "invalid ttl",
			ssh: `private_key_file: id
        certificate:
          backend: internal
          ttl: 10s
          internal:
            ca_key_file: /etc/ca`,
			contains: "between 1m and 24h",
		},
		{
			name: "internal without ca key",
			ssh: `private_key_file: id
        certificate:
          backend: internal`,
			contains: "requires internal.ca_key_file",
		},
		{
			name: "vault without role",
			ssh: `private_key_file: id
        certificate:
          backend: vault
          vault:
            addr: https://vault.example.com
            role_id: id
            secret_id: secret`,
			contains: "vault.role is required",
		},
		{
			name: "vault without app role credentials",
			ssh: `private_key_file: id
        certificate:
          backend: vault
          vault:
            addr: https://vault.example.com
            role: strangle`,
			contains: "requires role_id/secret_id",
		},
	}
	for _, tc := range cases {
		It(tc.name, func() {
			strangle := "playbooks:\n  - name: site\n    playbook: site.yml\n    inventory: inventory\n    options:\n      ssh:\n        enabled: true\n        " +
				tc.ssh + "\n"
			serverPath, collectionPath, err := writeTree("listen_addr: \":9000\"\n", collOne, strangle)
			Expect(err).NotTo(HaveOccurred())
			_, err = Load(serverPath, collectionPath)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(tc.contains))
		})
	}
})
