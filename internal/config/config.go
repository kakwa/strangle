// Package config loads strangle's runtime configuration from three kinds of
// YAML files:
//
//   - server.yml: process-level settings (listen address, log level, run
//     interval, database, auth placeholder).
//   - collection.yml: the inventory of Ansible projects (targets) that
//     strangle applies, with a git-upstream placeholder per target.
//   - strangle.yml: one per target, at the root of the project directory,
//     holding that project's playbooks, inventory, user, become options and
//     SSH transport.
//
// Configuration is file-only: there are no environment variables.
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
	// Server holds process-level settings (server.yml).
	Server ServerConfig
	// Targets are the Ansible projects to apply, each resolved from a
	// collection.yml entry and its directory's strangle.yml.
	Targets []TargetConfig
}

// Target returns the target with the given name.
func (c Config) Target(name string) (TargetConfig, bool) {
	for _, t := range c.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return TargetConfig{}, false
}

// ServerConfig is the content of server.yml.
type ServerConfig struct {
	// ListenAddr is the address the HTTP server listens on. Defaults to
	// ":8080".
	ListenAddr string `yaml:"listen_addr,omitempty"`
	// LogLevel is the log level: debug, info, warn or error. Defaults to
	// "info".
	LogLevel string `yaml:"log_level,omitempty"`
	// Interval is how often the applier runs a full cycle over every target.
	// Defaults to 15m.
	Interval time.Duration `yaml:"interval,omitempty"`
	// Database configures the run-history database.
	Database DatabaseConfig `yaml:"database,omitempty"`
	// Auth configures API authentication. Placeholder: parsed and validated
	// but not enforced yet.
	Auth AuthConfig `yaml:"auth,omitempty"`
}

// AuthConfig is a placeholder for future API authentication. It is parsed so
// that server.yml can carry the section from the start, but no enforcement
// happens yet.
type AuthConfig struct {
	// Token, if set, will have to be presented as a Bearer token on API
	// calls. Not enforced yet.
	Token string `yaml:"token,omitempty"`
}

// DatabaseConfig configures the PostgreSQL database that stores run history.
type DatabaseConfig struct {
	// DSN is the PostgreSQL connection string. Without a DSN strangle falls
	// back to an in-memory store and history is lost on restart.
	DSN string `yaml:"dsn,omitempty"`
}

// CollectionConfig is the content of collection.yml: the inventory of
// Ansible projects (targets) that strangle applies.
type CollectionConfig struct {
	// Targets is the ordered list of projects to apply.
	Targets []TargetSpec `yaml:"targets"`
}

// TargetSpec is one collection.yml entry. It points at the directory of an
// Ansible project, which must contain a strangle.yml with the project's
// playbook and SSH settings.
type TargetSpec struct {
	// Name is the target's identifier within the collection.
	Name string `yaml:"name"`
	// Path is the directory of the Ansible project (the directory that
	// contains strangle.yml).
	Path string `yaml:"path"`
	// Git describes the upstream repository of the project. Placeholder for
	// future sync: parsed and validated, but not used yet.
	Git GitConfig `yaml:"git,omitempty"`
}

// GitConfig describes the upstream git repository of a target. Parsed but not
// used yet: git sync is a placeholder.
type GitConfig struct {
	// URL is the repository URL.
	URL string `yaml:"url,omitempty"`
	// Ref is a branch, tag or commit to track.
	Ref string `yaml:"ref,omitempty"`
	// Branch is the tracked branch; shorthand for Ref when Ref is empty.
	Branch string `yaml:"branch,omitempty"`
}

// TargetConfig is one Ansible project: the collection.yml entry merged with
// its directory's strangle.yml.
type TargetConfig struct {
	// Name is the target's identifier from the collection.
	Name string `yaml:"-"`
	// Path is the resolved directory of the Ansible project.
	Path string `yaml:"-"`
	// Git is the upstream repository placeholder from the collection.
	Git GitConfig `yaml:"-"`

	// The fields below come from the target's strangle.yml.

	// WorkingDir is the directory the playbooks run from. Ansible resolves
	// ansible.cfg, relative paths and roles relative to it. Defaults to
	// Path.
	WorkingDir string `yaml:"working_dir,omitempty"`
	// Playbooks is the ordered list of playbook files to run, relative to
	// WorkingDir (or absolute).
	Playbooks []string `yaml:"playbooks"`
	// Inventory is the inventory file or directory, relative to WorkingDir
	// (or absolute).
	Inventory string `yaml:"inventory"`
	// User is the SSH user, overriding ansible_user in the inventory. It is
	// also the fallback user for the Go-managed SSH transport when the
	// inventory sets none.
	User string `yaml:"user,omitempty"`
	// Become enables privilege escalation.
	Become bool `yaml:"become,omitempty"`
	// BecomeMethod is the escalation method (sudo, su, ...). Only used when
	// Become is enabled; Ansible defaults it to sudo.
	BecomeMethod string `yaml:"become_method,omitempty"`
	// BecomeUser is the escalation target user. Only used when Become is
	// enabled.
	BecomeUser string `yaml:"become_user,omitempty"`
	// BecomePasswordFile holds the become password. Only used when Become
	// is enabled.
	BecomePasswordFile string `yaml:"become_password_file,omitempty"`
	// ConnectionPasswordFile holds the SSH password.
	ConnectionPasswordFile string `yaml:"connection_password_file,omitempty"`
	// VaultPasswordFile holds the Ansible vault password.
	VaultPasswordFile string `yaml:"vault_password_file,omitempty"`
	// SSH configures the Go-managed SSH transport used by Ansible for this
	// target.
	SSH SSHConfig `yaml:"ssh,omitempty"`
}

// HostKeyPolicy controls how unknown SSH host keys are handled.
type HostKeyPolicy string

const (
	// PolicyKnownHosts verifies against a classic known_hosts file.
	PolicyKnownHosts HostKeyPolicy = "known_hosts"
	// PolicyAPI denies unknown keys until they are approved over the API/UI.
	PolicyAPI HostKeyPolicy = "api"
	// PolicyCA trusts host certificates signed by a configured CA.
	PolicyCA HostKeyPolicy = "ca"
	// PolicyAuto trusts the first key seen for a host and rejects later changes.
	PolicyAuto HostKeyPolicy = "auto"
	// PolicyInsecure accepts any host key. For development only.
	PolicyInsecure HostKeyPolicy = "insecure"
)

// Valid reports whether p is a known host key policy.
func (p HostKeyPolicy) Valid() bool {
	switch p {
	case PolicyKnownHosts, PolicyAPI, PolicyCA, PolicyAuto, PolicyInsecure:
		return true
	}
	return false
}

// SSHConfig configures the persistent SSH connections owned by the Go process
// on behalf of one target.
type SSHConfig struct {
	Enabled bool `yaml:"enabled,omitempty"`
	// User is the SSH user for connections the inventory does not set.
	// Defaults to the target-level user key.
	User string `yaml:"user,omitempty"`
	Port int `yaml:"port,omitempty"`
	PrivateKeyFile       string        `yaml:"private_key_file,omitempty"`
	PrivateKeyPassphrase string        `yaml:"private_key_passphrase,omitempty"`
	UseAgent             bool          `yaml:"use_agent,omitempty"`
	Password             string        `yaml:"password,omitempty"`
	HostKeyPolicy        HostKeyPolicy `yaml:"host_key_policy,omitempty"`
	KnownHostsFile       string        `yaml:"known_hosts_file,omitempty"`
	CAFiles              []string      `yaml:"ca_files,omitempty"`
	AutoAcceptFile       string        `yaml:"auto_accept_file,omitempty"`
	PendingFile          string        `yaml:"pending_file,omitempty"`
}

// Load reads the server.yml and collection.yml files and resolves each
// target's strangle.yml, applying defaults.
func Load(serverPath, collectionPath string) (Config, error) {
	var cfg Config

	data, err := os.ReadFile(serverPath)
	if err != nil {
		return Config{}, fmt.Errorf("read server config %q: %w", serverPath, err)
	}
	if err := yaml.Unmarshal(data, &cfg.Server); err != nil {
		return Config{}, fmt.Errorf("parse server config %q: %w", serverPath, err)
	}

	data, err = os.ReadFile(collectionPath)
	if err != nil {
		return Config{}, fmt.Errorf("read collection %q: %w", collectionPath, err)
	}
	var coll CollectionConfig
	if err := yaml.Unmarshal(data, &coll); err != nil {
		return Config{}, fmt.Errorf("parse collection %q: %w", collectionPath, err)
	}
	if len(coll.Targets) == 0 {
		return Config{}, fmt.Errorf("collection %q: targets is required (non-empty list)", collectionPath)
	}

	// Validate every spec first so collection-level problems surface before
	// any target file is read.
	seen := map[string]struct{}{}
	for _, spec := range coll.Targets {
		if spec.Name == "" {
			return Config{}, errors.New("collection: every target needs a name")
		}
		if _, dup := seen[spec.Name]; dup {
			return Config{}, fmt.Errorf("collection: duplicate target name %q", spec.Name)
		}
		seen[spec.Name] = struct{}{}
		if spec.Path == "" {
			return Config{}, fmt.Errorf("collection: target %q needs a path", spec.Name)
		}
	}

	// Relative target paths resolve against the collection file's directory
	// (not the working directory), so a collection.yml can be placed next to
	// the targets it lists.
	collDir := filepath.Dir(collectionPath)

	cfg.Targets = make([]TargetConfig, 0, len(coll.Targets))
	for _, spec := range coll.Targets {
		p := spec.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(collDir, p)
		}
		path, err := filepath.Abs(p)
		if err != nil {
			return Config{}, fmt.Errorf("collection: target %q: %w", spec.Name, err)
		}
		t, err := loadTarget(spec, path)
		if err != nil {
			return Config{}, err
		}
		cfg.Targets = append(cfg.Targets, t)
	}

	if err := cfg.Server.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadTarget reads and validates the strangle.yml in the target directory.
func loadTarget(spec TargetSpec, path string) (TargetConfig, error) {
	data, err := os.ReadFile(filepath.Join(path, "strangle.yml"))
	if err != nil {
		return TargetConfig{}, fmt.Errorf("target %q: read %q: %w", spec.Name, filepath.Join(path, "strangle.yml"), err)
	}
	var t TargetConfig
	if err := yaml.Unmarshal(data, &t); err != nil {
		return TargetConfig{}, fmt.Errorf("target %q: parse strangle.yml: %w", spec.Name, err)
	}
	t.Name = spec.Name
	t.Path = path
	t.Git = spec.Git
	if err := t.validate(); err != nil {
		return TargetConfig{}, err
	}
	return t, nil
}

// validate checks the server configuration and applies defaults.
func (s *ServerConfig) validate() error {
	if s.ListenAddr == "" {
		s.ListenAddr = ":8080"
	}
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	switch s.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("server: log_level must be one of debug, info, warn, error (got %q)", s.LogLevel)
	}
	if s.Interval <= 0 {
		s.Interval = 15 * time.Minute
	}
	return nil
}

// validate checks the target configuration and applies defaults that depend
// on other settings.
func (t *TargetConfig) validate() error {
	if len(t.Playbooks) == 0 {
		return fmt.Errorf("target %q: playbooks is required (ordered list of playbook files)", t.Name)
	}
	if t.Inventory == "" {
		return fmt.Errorf("target %q: inventory is required", t.Name)
	}
	if t.WorkingDir == "" {
		t.WorkingDir = t.Path
	} else if !filepath.IsAbs(t.WorkingDir) {
		t.WorkingDir = filepath.Join(t.Path, t.WorkingDir)
	}

	if !t.SSH.Enabled {
		return nil
	}
	if t.SSH.User == "" {
		t.SSH.User = t.User
	}
	if t.SSH.Port == 0 {
		t.SSH.Port = 22
	}
	if t.SSH.HostKeyPolicy == "" {
		t.SSH.HostKeyPolicy = PolicyAPI
	}
	if !t.SSH.HostKeyPolicy.Valid() {
		return fmt.Errorf("target %q: ssh.host_key_policy must be one of %s, %s, %s, %s, %s (got %q)",
			t.Name, PolicyAPI, PolicyKnownHosts, PolicyCA, PolicyAuto, PolicyInsecure, t.SSH.HostKeyPolicy)
	}
	if t.SSH.HostKeyPolicy == PolicyCA && len(t.SSH.CAFiles) == 0 {
		return fmt.Errorf("target %q: ssh.host_key_policy ca requires at least one ssh.ca_files entry", t.Name)
	}
	if t.SSH.KnownHostsFile == "" {
		t.SSH.KnownHostsFile = filepath.Join(t.Path, "known_hosts.strangle")
	}
	if t.SSH.AutoAcceptFile == "" {
		t.SSH.AutoAcceptFile = filepath.Join(t.Path, "known_hosts.auto.json")
	}
	if t.SSH.PendingFile == "" {
		t.SSH.PendingFile = filepath.Join(t.Path, "hostkeys.pending.json")
	}
	return nil
}

// PlaybookPath resolves a playbook name against WorkingDir.
func (t TargetConfig) PlaybookPath(playbook string) string {
	if filepath.IsAbs(playbook) {
		return playbook
	}
	return filepath.Join(t.WorkingDir, playbook)
}

// InventoryPath resolves the inventory against WorkingDir.
func (t TargetConfig) InventoryPath() string {
	if filepath.IsAbs(t.Inventory) {
		return t.Inventory
	}
	return filepath.Join(t.WorkingDir, t.Inventory)
}

// Args builds the ansible-playbook argument list for the given playbook.
// --diff is always included. The become flags are only passed when become is
// enabled.
func (t TargetConfig) Args(playbook string) []string {
	args := []string{"--diff", "-i", t.Inventory}

	if t.User != "" {
		args = append(args, "-u", t.User)
	}
	if t.Become {
		args = append(args, "-b")
		if t.BecomeMethod != "" {
			args = append(args, "--become-method", t.BecomeMethod)
		}
		if t.BecomeUser != "" {
			args = append(args, "--become-user", t.BecomeUser)
		}
		if t.BecomePasswordFile != "" {
			args = append(args, "--become-password-file", t.BecomePasswordFile)
		}
	}
	if t.ConnectionPasswordFile != "" {
		args = append(args, "--connection-password-file", t.ConnectionPasswordFile)
	}
	if t.VaultPasswordFile != "" {
		args = append(args, "--vault-password-file", t.VaultPasswordFile)
	}

	return append(args, playbook)
}
