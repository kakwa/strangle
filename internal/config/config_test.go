package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	path := writeConfig(t, `
playbook: site.yml
inventory: hosts.ini
user: deploy
become: true
become_method: su
become_user: root
become_password_file: /run/secrets/become
connection_password_file: /run/secrets/conn
vault_password_file: /run/secrets/vault
`)
	t.Setenv("STRANGLE_CONFIG", path)
	t.Setenv("STRANGLE_ADDR", ":9000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.ListenAddr != ":9000" {
		t.Errorf("ListenAddr = %q, want :9000", cfg.ListenAddr)
	}
	if cfg.Interval <= 0 {
		t.Errorf("Interval = %v, want a positive default", cfg.Interval)
	}
	if cfg.Ansible.Playbook != "site.yml" || cfg.Ansible.Inventory != "hosts.ini" {
		t.Errorf("playbook/inventory = %q/%q", cfg.Ansible.Playbook, cfg.Ansible.Inventory)
	}

	args := strings.Join(cfg.Ansible.Args(), " ")
	for _, want := range []string{
		"--diff",
		"-i hosts.ini",
		"-u deploy",
		"-b",
		"--become-method su",
		"--become-user root",
		"--become-password-file /run/secrets/become",
		"--connection-password-file /run/secrets/conn",
		"--vault-password-file /run/secrets/vault",
		"site.yml",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("Args() = %q, missing %q", args, want)
		}
	}
}

func TestLoadRequiresPlaybookAndInventory(t *testing.T) {
	tests := map[string]string{
		"playbook":  "inventory: hosts.ini\n",
		"inventory": "playbook: site.yml\n",
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("STRANGLE_CONFIG", writeConfig(t, data))
			if _, err := Load(); err == nil {
				t.Fatal("Load() succeeded, want an error")
			}
		})
	}
}

func TestArgsAlwaysAddsDiffAndDefaultsBecomeMethod(t *testing.T) {
	path := writeConfig(t, "playbook: site.yml\ninventory: hosts.ini\n")
	t.Setenv("STRANGLE_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Ansible.BecomeMethod != "sudo" {
		t.Errorf("BecomeMethod = %q, want sudo", cfg.Ansible.BecomeMethod)
	}
	if !slices.Contains(cfg.Ansible.Args(), "--diff") {
		t.Errorf("Args() = %v, want --diff", cfg.Ansible.Args())
	}
}

func TestLoadSSHDefaults(t *testing.T) {
	path := writeConfig(t, "playbook: site.yml\ninventory: hosts.ini\n")
	t.Setenv("STRANGLE_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.SSH.Enabled {
		t.Error("SSH.Enabled = true, want false by default")
	}
	if cfg.SSH.Port != 22 {
		t.Errorf("SSH.Port = %d, want 22", cfg.SSH.Port)
	}
	if cfg.SSH.HostKeyPolicy != "api" {
		t.Errorf("SSH.HostKeyPolicy = %q, want api", cfg.SSH.HostKeyPolicy)
	}
	if cfg.SSH.KnownHostsFile == "" || cfg.SSH.AutoAcceptFile == "" || cfg.SSH.PendingFile == "" {
		t.Error("SSH file paths not defaulted")
	}
}

func TestLoadSSHConfig(t *testing.T) {
	path := writeConfig(t, `
playbook: site.yml
inventory: hosts.ini
ssh:
  enabled: true
  user: deploy
  port: 2222
  private_key_file: /keys/id_ed25519
  use_agent: true
  host_key_policy: auto
  ca_files:
    - /keys/ca.pub
`)
	t.Setenv("STRANGLE_CONFIG", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if !cfg.SSH.Enabled || cfg.SSH.User != "deploy" || cfg.SSH.Port != 2222 {
		t.Errorf("SSH settings = %+v", cfg.SSH)
	}
	if cfg.SSH.PrivateKeyFile != "/keys/id_ed25519" || !cfg.SSH.UseAgent {
		t.Errorf("SSH auth settings = %+v", cfg.SSH)
	}
	if cfg.SSH.HostKeyPolicy != "auto" || len(cfg.SSH.CAFiles) != 1 {
		t.Errorf("SSH host key settings = %+v", cfg.SSH)
	}
}

func writeConfig(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "strangle.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
