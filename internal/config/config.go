// Package config holds the runtime configuration for strangle,
// loaded from a YAML file and environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all runtime configuration for strangle.
type Config struct {
	// ListenAddr is the address the HTTP server listens on.
	ListenAddr string `yaml:"-"`
	// Interval is how often the applier runs the full playbook list.
	Interval time.Duration `yaml:"-"`
	// Ansible holds the options used to build the ansible-playbook command line.
	Ansible AnsibleConfig `yaml:",inline"`
	// SSH configures the Go-managed SSH transport used by Ansible.
	SSH SSHConfig `yaml:"ssh,omitempty"`
	// Database configures the run-history database.
	Database DatabaseConfig `yaml:"database,omitempty"`
}

// DatabaseConfig configures the PostgreSQL database that stores run history.
type DatabaseConfig struct {
	// DSN is the PostgreSQL connection string. It can be set here or with
	// the STRANGLE_DSN environment variable (which wins). Without a DSN
	// strangle falls back to an in-memory store.
	DSN string `yaml:"dsn,omitempty"`
}

// SSHConfig configures the persistent SSH connections owned by the Go process.
type SSHConfig struct {
	Enabled              bool     `yaml:"enabled,omitempty"`
	User                 string   `yaml:"user,omitempty"`
	Port                 int      `yaml:"port,omitempty"`
	PrivateKeyFile       string   `yaml:"private_key_file,omitempty"`
	PrivateKeyPassphrase string   `yaml:"private_key_passphrase,omitempty"`
	UseAgent             bool     `yaml:"use_agent,omitempty"`
	Password             string   `yaml:"password,omitempty"`
	HostKeyPolicy        string   `yaml:"host_key_policy,omitempty"`
	KnownHostsFile       string   `yaml:"known_hosts_file,omitempty"`
	CAFiles              []string `yaml:"ca_files,omitempty"`
	AutoAcceptFile       string   `yaml:"auto_accept_file,omitempty"`
	PendingFile          string   `yaml:"pending_file,omitempty"`
}

// AnsibleConfig describes how ansible-playbook is invoked. Playbooks and
// inventory are required; everything else is optional. Password options take
// file paths because an embedded interpreter cannot answer interactive prompts.
type AnsibleConfig struct {
	// WorkingDir is the directory the playbooks run from. Ansible resolves
	// ansible.cfg, relative paths and roles relative to it, so it should be
	// the root of the Ansible project.
	WorkingDir string `yaml:"working_dir,omitempty"`
	// Playbooks is the ordered list of playbook files to run, relative to
	// WorkingDir (or absolute).
	Playbooks []string `yaml:"playbooks,omitempty"`
	// Playbook is a legacy single-playbook form, equivalent to
	// playbooks: [playbook].
	Playbook string `yaml:"playbook,omitempty"`
	// Inventory is the inventory file or directory, relative to WorkingDir
	// (or absolute).
	Inventory string `yaml:"inventory"`
	// User is the SSH user, overriding ansible_user in the inventory.
	User string `yaml:"user,omitempty"`
	// Become enables privilege escalation.
	Become bool `yaml:"become,omitempty"`
	// BecomeMethod is the escalation method (sudo, su, ...).
	BecomeMethod string `yaml:"become_method,omitempty"`
	// BecomeUser is the escalation target user.
	BecomeUser string `yaml:"become_user,omitempty"`
	// BecomePasswordFile holds the become password.
	BecomePasswordFile string `yaml:"become_password_file,omitempty"`
	// ConnectionPasswordFile holds the SSH password.
	ConnectionPasswordFile string `yaml:"connection_password_file,omitempty"`
	// VaultPasswordFile holds the Ansible vault password.
	VaultPasswordFile string `yaml:"vault_password_file,omitempty"`
}

// PlaybookList returns the effective playbook list: the playbooks list when
// non-empty, otherwise the legacy single playbook.
func (a AnsibleConfig) PlaybookList() []string {
	if len(a.Playbooks) > 0 {
		return a.Playbooks
	}
	if a.Playbook != "" {
		return []string{a.Playbook}
	}
	return nil
}

// PlaybookPath resolves a playbook name against WorkingDir.
func (a AnsibleConfig) PlaybookPath(playbook string) string {
	if filepath.IsAbs(playbook) {
		return playbook
	}
	if a.WorkingDir != "" {
		return filepath.Join(a.WorkingDir, playbook)
	}
	return playbook
}

// InventoryPath resolves the inventory against WorkingDir.
func (a AnsibleConfig) InventoryPath() string {
	if filepath.IsAbs(a.Inventory) {
		return a.Inventory
	}
	if a.WorkingDir != "" {
		return filepath.Join(a.WorkingDir, a.Inventory)
	}
	return a.Inventory
}

// Load reads configuration from the environment and the YAML config file,
// applying defaults. The config file path defaults to "strangle.yaml" and can
// be changed with STRANGLE_CONFIG.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr: env("STRANGLE_ADDR", ":8080"),
		Interval:   envDuration("STRANGLE_INTERVAL", 15*time.Minute),
	}

	path := env("STRANGLE_CONFIG", "strangle.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	if len(cfg.Ansible.PlaybookList()) == 0 {
		return Config{}, errors.New("config: playbook(s) are required (set playbooks or playbook)")
	}
	if cfg.Ansible.Inventory == "" {
		return Config{}, errors.New("config: inventory is required")
	}
	if cfg.Ansible.BecomeMethod == "" {
		cfg.Ansible.BecomeMethod = "sudo"
	}

	if dsn := os.Getenv("STRANGLE_DSN"); dsn != "" {
		cfg.Database.DSN = dsn
	}

	if cfg.SSH.Port == 0 {
		cfg.SSH.Port = 22
	}
	if cfg.SSH.HostKeyPolicy == "" {
		cfg.SSH.HostKeyPolicy = "api"
	}
	if cfg.SSH.KnownHostsFile == "" {
		cfg.SSH.KnownHostsFile = "known_hosts.strangle"
	}
	if cfg.SSH.AutoAcceptFile == "" {
		cfg.SSH.AutoAcceptFile = "known_hosts.auto.json"
	}
	if cfg.SSH.PendingFile == "" {
		cfg.SSH.PendingFile = "hostkeys.pending.json"
	}

	return cfg, nil
}

// Args builds the ansible-playbook argument list for the given playbook.
// --diff is always included.
func (a AnsibleConfig) Args(playbook string) []string {
	args := []string{"--diff", "-i", a.Inventory}

	if a.User != "" {
		args = append(args, "-u", a.User)
	}
	if a.Become {
		args = append(args, "-b")
	}
	if a.BecomeMethod != "" {
		args = append(args, "--become-method", a.BecomeMethod)
	}
	if a.BecomeUser != "" {
		args = append(args, "--become-user", a.BecomeUser)
	}
	if a.BecomePasswordFile != "" {
		args = append(args, "--become-password-file", a.BecomePasswordFile)
	}
	if a.ConnectionPasswordFile != "" {
		args = append(args, "--connection-password-file", a.ConnectionPasswordFile)
	}
	if a.VaultPasswordFile != "" {
		args = append(args, "--vault-password-file", a.VaultPasswordFile)
	}

	return append(args, playbook)
}

// ArgsLegacy builds the argument list for the legacy single-playbook config.
func (a AnsibleConfig) ArgsLegacy() []string {
	pbs := a.PlaybookList()
	if len(pbs) == 0 {
		return nil
	}
	return a.Args(pbs[0])
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
