// Package config loads strangle's runtime configuration from three kinds of
// YAML files:
//
//   - server.yml: process-level settings (listen address, log level, run
//     interval, database, auth placeholder).
//   - collection.yml: the inventory of Ansible projects (targets) that
//     strangle applies, with a git-upstream placeholder per target.
//   - strangle.yml: one per target, at the root of the project directory.
//     Each entry of its `playbooks` list is a named job with its own
//     playbook file, inventory and Ansible/SSH options.
//
// Configuration is file-only: there are no environment variables.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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
// Ansible project, which must contain a strangle.yml with named playbooks.
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
// the named playbooks of its directory's strangle.yml.
type TargetConfig struct {
	// Name is the target's identifier from the collection.
	Name string
	// Path is the resolved directory of the Ansible project.
	Path string
	// Git is the upstream repository placeholder from the collection.
	Git GitConfig

	// WorkingDir is the directory the playbooks run from. Ansible resolves
	// ansible.cfg, relative paths and roles relative to it. Defaults to
	// Path.
	WorkingDir string

	// Playbooks are the resolved, ordered jobs of this target.
	Playbooks []PlaybookConfig
}

// Playbook returns the named playbook of the target.
func (t TargetConfig) Playbook(name string) (PlaybookConfig, bool) {
	for _, pb := range t.Playbooks {
		if pb.Name == name {
			return pb, true
		}
	}
	return PlaybookConfig{}, false
}

// targetFile is the on-disk shape of a strangle.yml. Strict decoding of this
// struct rejects the old flat format (a list of file names plus a target-wide
// inventory) as well as misspelled keys.
type targetFile struct {
	WorkingDir string         `yaml:"working_dir,omitempty"`
	Options    OptionsSpec    `yaml:"options,omitempty"`
	Playbooks  []PlaybookSpec `yaml:"playbooks"`
}

// PlaybookSpec is one entry of the `playbooks` list in strangle.yml.
type PlaybookSpec struct {
	// Name identifies the job in the API, web UI and run history.
	Name string `yaml:"name"`
	// Playbook is the playbook file to execute, relative to working_dir
	// (or absolute).
	Playbook string `yaml:"playbook"`
	// Inventory is the inventory file or directory, relative to working_dir
	// (or absolute).
	Inventory string `yaml:"inventory"`
	// Options override the target-level `options` defaults for this job.
	Options OptionsSpec `yaml:"options,omitempty"`
}

// OptionsSpec is the `options` block, both as target-level defaults and as a
// per-playbook override. Pointer scalars distinguish "unset" from false/0 so
// defaults can be overridden explicitly.
type OptionsSpec struct {
	User                   string   `yaml:"user,omitempty"`
	Become                 *bool    `yaml:"become,omitempty"`
	BecomeMethod           string   `yaml:"become_method,omitempty"`
	BecomeUser             string   `yaml:"become_user,omitempty"`
	BecomePasswordFile     string   `yaml:"become_password_file,omitempty"`
	ConnectionPasswordFile string   `yaml:"connection_password_file,omitempty"`
	VaultPasswordFile      string   `yaml:"vault_password_file,omitempty"`
	SSH                    *SSHSpec `yaml:"ssh,omitempty"`
}

// SSHSpec is the YAML shape of the SSH transport options.
type SSHSpec struct {
	Enabled              *bool         `yaml:"enabled,omitempty"`
	User                 string        `yaml:"user,omitempty"`
	Port                 *int          `yaml:"port,omitempty"`
	PrivateKeyFile       string        `yaml:"private_key_file,omitempty"`
	PrivateKeyPassphrase string        `yaml:"private_key_passphrase,omitempty"`
	UseAgent             *bool         `yaml:"use_agent,omitempty"`
	Password             string        `yaml:"password,omitempty"`
	HostKeyPolicy        HostKeyPolicy `yaml:"host_key_policy,omitempty"`
	KnownHostsFile       string        `yaml:"known_hosts_file,omitempty"`
	CAFiles              []string      `yaml:"ca_files,omitempty"`
	AutoAcceptFile       string        `yaml:"auto_accept_file,omitempty"`
	PendingFile          string        `yaml:"pending_file,omitempty"`
}

// PlaybookOptions holds the resolved Ansible and SSH options of one playbook.
type PlaybookOptions struct {
	User                   string
	Become                 bool
	BecomeMethod           string
	BecomeUser             string
	BecomePasswordFile     string
	ConnectionPasswordFile string
	VaultPasswordFile      string
	SSH                    SSHConfig
}

// PlaybookConfig is a validated, resolved entry of a target's playbooks list.
type PlaybookConfig struct {
	// Name is the job identifier used by the API, UI and run history.
	Name string
	// TargetName is the collection target this job belongs to.
	TargetName string
	// TargetPath is the target's project directory, used for default SSH
	// host-key files.
	TargetPath string
	// Playbook is the configured playbook path (relative to WorkingDir).
	Playbook string
	// Inventory is the configured inventory path (relative to WorkingDir).
	Inventory string
	// WorkingDir is the absolute directory the job runs from.
	WorkingDir string
	// Options are the merged target-level and per-playbook options.
	Options PlaybookOptions
}

// SSHKey identifies this playbook's SSH manager. Managers are scoped per job
// so that different playbooks can use different credentials and host-key
// policies; the same key is used by the host-key approval API.
func (p PlaybookConfig) SSHKey() string {
	return p.TargetName + "/" + p.Name
}

// PlaybookPath resolves the playbook file against WorkingDir.
func (p PlaybookConfig) PlaybookPath() string {
	if filepath.IsAbs(p.Playbook) {
		return p.Playbook
	}
	return filepath.Join(p.WorkingDir, p.Playbook)
}

// InventoryPath resolves the inventory against WorkingDir.
func (p PlaybookConfig) InventoryPath() string {
	if filepath.IsAbs(p.Inventory) {
		return p.Inventory
	}
	return filepath.Join(p.WorkingDir, p.Inventory)
}

// Args builds the ansible-playbook argument list for this job. --diff is
// always included; the become flags are only passed when become is enabled.
func (p PlaybookConfig) Args() []string {
	args := []string{"--diff", "-i", p.Inventory}

	if p.Options.User != "" {
		args = append(args, "-u", p.Options.User)
	}
	if p.Options.Become {
		args = append(args, "-b")
		if p.Options.BecomeMethod != "" {
			args = append(args, "--become-method", p.Options.BecomeMethod)
		}
		if p.Options.BecomeUser != "" {
			args = append(args, "--become-user", p.Options.BecomeUser)
		}
		if p.Options.BecomePasswordFile != "" {
			args = append(args, "--become-password-file", p.Options.BecomePasswordFile)
		}
	}
	if p.Options.ConnectionPasswordFile != "" {
		args = append(args, "--connection-password-file", p.Options.ConnectionPasswordFile)
	}
	if p.Options.VaultPasswordFile != "" {
		args = append(args, "--vault-password-file", p.Options.VaultPasswordFile)
	}

	return append(args, p.Playbook)
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
// for one playbook.
type SSHConfig struct {
	Enabled              bool          `yaml:"enabled,omitempty"`
	User                 string        `yaml:"user,omitempty"`
	Port                 int           `yaml:"port,omitempty"`
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
	var file targetFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil && !errors.Is(err, io.EOF) {
		return TargetConfig{}, fmt.Errorf("target %q: parse strangle.yml: %w", spec.Name, err)
	}
	if len(file.Playbooks) == 0 {
		return TargetConfig{}, fmt.Errorf("target %q: playbooks is required (non-empty list of named jobs)", spec.Name)
	}

	t := TargetConfig{
		Name: spec.Name,
		Path: path,
		Git:  spec.Git,
	}
	if file.WorkingDir == "" {
		t.WorkingDir = path
	} else if filepath.IsAbs(file.WorkingDir) {
		t.WorkingDir = file.WorkingDir
	} else {
		t.WorkingDir = filepath.Join(path, file.WorkingDir)
	}

	seen := map[string]struct{}{}
	for _, ps := range file.Playbooks {
		if ps.Name == "" {
			return TargetConfig{}, fmt.Errorf("target %q: every playbook needs a name", spec.Name)
		}
		if !validPlaybookName(ps.Name) {
			return TargetConfig{}, fmt.Errorf("target %q: invalid playbook name %q (use letters, digits, '.', '_' or '-')", spec.Name, ps.Name)
		}
		if _, dup := seen[ps.Name]; dup {
			return TargetConfig{}, fmt.Errorf("target %q: duplicate playbook name %q", spec.Name, ps.Name)
		}
		seen[ps.Name] = struct{}{}
		if ps.Playbook == "" {
			return TargetConfig{}, fmt.Errorf("target %q: playbook %q needs a playbook file", spec.Name, ps.Name)
		}
		if ps.Inventory == "" {
			return TargetConfig{}, fmt.Errorf("target %q: playbook %q needs an inventory", spec.Name, ps.Name)
		}

		options, err := resolveOptions(mergeOptions(file.Options, ps.Options), path, ps.Name)
		if err != nil {
			return TargetConfig{}, fmt.Errorf("target %q: playbook %q: %w", spec.Name, ps.Name, err)
		}
		t.Playbooks = append(t.Playbooks, PlaybookConfig{
			Name:       ps.Name,
			TargetName: spec.Name,
			TargetPath: path,
			Playbook:   ps.Playbook,
			Inventory:  ps.Inventory,
			WorkingDir: t.WorkingDir,
			Options:    options,
		})
	}
	return t, nil
}

// validPlaybookName rejects path separators and other characters that would
// make the name unsafe as an SSH-scope or host-key file suffix.
func validPlaybookName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// mergeOptions overlays per-playbook options on the target-level defaults.
func mergeOptions(base, over OptionsSpec) OptionsSpec {
	out := base
	if over.User != "" {
		out.User = over.User
	}
	if over.Become != nil {
		out.Become = over.Become
	}
	if over.BecomeMethod != "" {
		out.BecomeMethod = over.BecomeMethod
	}
	if over.BecomeUser != "" {
		out.BecomeUser = over.BecomeUser
	}
	if over.BecomePasswordFile != "" {
		out.BecomePasswordFile = over.BecomePasswordFile
	}
	if over.ConnectionPasswordFile != "" {
		out.ConnectionPasswordFile = over.ConnectionPasswordFile
	}
	if over.VaultPasswordFile != "" {
		out.VaultPasswordFile = over.VaultPasswordFile
	}
	if over.SSH != nil {
		if base.SSH == nil {
			out.SSH = over.SSH
		} else {
			merged := mergeSSH(*base.SSH, *over.SSH)
			out.SSH = &merged
		}
	}
	return out
}

// mergeSSH overlays a playbook's SSH options on the target-level SSH defaults.
func mergeSSH(base, over SSHSpec) SSHSpec {
	out := base
	if over.Enabled != nil {
		out.Enabled = over.Enabled
	}
	if over.User != "" {
		out.User = over.User
	}
	if over.Port != nil {
		out.Port = over.Port
	}
	if over.PrivateKeyFile != "" {
		out.PrivateKeyFile = over.PrivateKeyFile
	}
	if over.PrivateKeyPassphrase != "" {
		out.PrivateKeyPassphrase = over.PrivateKeyPassphrase
	}
	if over.UseAgent != nil {
		out.UseAgent = over.UseAgent
	}
	if over.Password != "" {
		out.Password = over.Password
	}
	if over.HostKeyPolicy != "" {
		out.HostKeyPolicy = over.HostKeyPolicy
	}
	if over.KnownHostsFile != "" {
		out.KnownHostsFile = over.KnownHostsFile
	}
	if len(over.CAFiles) > 0 {
		out.CAFiles = over.CAFiles
	}
	if over.AutoAcceptFile != "" {
		out.AutoAcceptFile = over.AutoAcceptFile
	}
	if over.PendingFile != "" {
		out.PendingFile = over.PendingFile
	}
	return out
}

// resolveOptions turns a YAML options block into concrete values and applies
// the SSH defaults. Host-key files are scoped per playbook so two jobs with
// different policies cannot overwrite each other's trust decisions.
func resolveOptions(spec OptionsSpec, targetPath, playbookName string) (PlaybookOptions, error) {
	o := PlaybookOptions{
		User:                   spec.User,
		Become:                 boolOr(spec.Become, false),
		BecomeMethod:           spec.BecomeMethod,
		BecomeUser:             spec.BecomeUser,
		BecomePasswordFile:     spec.BecomePasswordFile,
		ConnectionPasswordFile: spec.ConnectionPasswordFile,
		VaultPasswordFile:      spec.VaultPasswordFile,
	}
	if spec.SSH == nil {
		return o, nil
	}

	s := spec.SSH
	o.SSH = SSHConfig{
		Enabled:              boolOr(s.Enabled, false),
		User:                 s.User,
		Port:                 intOr(s.Port, 0),
		PrivateKeyFile:       s.PrivateKeyFile,
		PrivateKeyPassphrase: s.PrivateKeyPassphrase,
		UseAgent:             boolOr(s.UseAgent, false),
		Password:             s.Password,
		HostKeyPolicy:        s.HostKeyPolicy,
		KnownHostsFile:       s.KnownHostsFile,
		CAFiles:              s.CAFiles,
		AutoAcceptFile:       s.AutoAcceptFile,
		PendingFile:          s.PendingFile,
	}
	if !o.SSH.Enabled {
		return o, nil
	}
	if o.SSH.User == "" {
		o.SSH.User = o.User
	}
	if o.SSH.Port == 0 {
		o.SSH.Port = 22
	}
	if o.SSH.HostKeyPolicy == "" {
		o.SSH.HostKeyPolicy = PolicyAPI
	}
	if !o.SSH.HostKeyPolicy.Valid() {
		return o, fmt.Errorf("ssh.host_key_policy must be one of %s, %s, %s, %s, %s (got %q)",
			PolicyAPI, PolicyKnownHosts, PolicyCA, PolicyAuto, PolicyInsecure, o.SSH.HostKeyPolicy)
	}
	if o.SSH.HostKeyPolicy == PolicyCA && len(o.SSH.CAFiles) == 0 {
		return o, errors.New("ssh.host_key_policy ca requires at least one ssh.ca_files entry")
	}
	prefix := filepath.Join(targetPath, "strangle."+playbookName)
	if o.SSH.KnownHostsFile == "" {
		o.SSH.KnownHostsFile = prefix + ".known_hosts"
	}
	if o.SSH.AutoAcceptFile == "" {
		o.SSH.AutoAcceptFile = prefix + ".auto.json"
	}
	if o.SSH.PendingFile == "" {
		o.SSH.PendingFile = prefix + ".pending.json"
	}
	return o, nil
}

func boolOr(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

func intOr(p *int, fallback int) int {
	if p == nil {
		return fallback
	}
	return *p
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
