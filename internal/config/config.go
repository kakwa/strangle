// Package config holds the runtime configuration for strangle,
// loaded from a YAML file and environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all runtime configuration for strangle.
type Config struct {
	// ListenAddr is the address the HTTP server listens on.
	ListenAddr string `yaml:"-"`
	// Interval is how often the applier runs over the job list.
	Interval time.Duration `yaml:"-"`
	// Ansible holds the options used to build the ansible-playbook command line.
	Ansible AnsibleConfig `yaml:",inline"`
	// SSH configures the Go-managed SSH transport used by Ansible.
	SSH SSHConfig `yaml:"ssh,omitempty"`
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

// AnsibleConfig describes how ansible-playbook is invoked. The playbook and
// inventory are required; everything else is optional. Password options take
// file paths because an embedded interpreter cannot answer interactive prompts.
type AnsibleConfig struct {
	Playbook               string `yaml:"playbook"`
	Inventory              string `yaml:"inventory"`
	User                   string `yaml:"user,omitempty"`
	Become                 bool   `yaml:"become,omitempty"`
	BecomeMethod           string `yaml:"become_method,omitempty"`
	BecomeUser             string `yaml:"become_user,omitempty"`
	BecomePasswordFile     string `yaml:"become_password_file,omitempty"`
	ConnectionPasswordFile string `yaml:"connection_password_file,omitempty"`
	VaultPasswordFile      string `yaml:"vault_password_file,omitempty"`
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

	if cfg.Ansible.Playbook == "" {
		return Config{}, errors.New("config: playbook is required")
	}
	if cfg.Ansible.Inventory == "" {
		return Config{}, errors.New("config: inventory is required")
	}
	if cfg.Ansible.BecomeMethod == "" {
		cfg.Ansible.BecomeMethod = "sudo"
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

// Args builds the ansible-playbook argument list. --diff is always included.
func (a AnsibleConfig) Args() []string {
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

	return append(args, a.Playbook)
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
