// Package config loads strangle's runtime configuration from three kinds of
// YAML files:
//
//   - server.yml: process-level settings (listen address, log level, default
//     run interval, database, auth placeholder).
//   - collection.yml: the inventory of Ansible projects (targets) that
//     strangle applies; a target may carry a git upstream that strangle
//     clones and keeps in sync.
//   - strangle.yml: one per target, at the root of the project directory.
//     Each entry of its `playbooks` list is a named job with its own
//     playbook file, inventory, schedule (duration or cron) and Ansible/SSH
//     options.
//
// Configuration is file-only: there are no environment variables.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	sched "github.com/kakwa/strangle/internal/schedule"
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

// DefaultOrg is the organization used when a collection.yml target does not
// name one. It keeps single-organization installations simple: all their
// collections live under /api/v1/orgs/default/collections/....
const DefaultOrg = "default"

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
	// Interval is the fallback schedule for jobs that do not set one in
	// strangle.yml. It preserves the original single-interval behaviour.
	// Defaults to 15m.
	Interval time.Duration `yaml:"interval,omitempty"`
	// Database configures the run-history database.
	Database DatabaseConfig `yaml:"database,omitempty"`
	// Workers is how many worker child processes the master spawns to
	// execute playbook jobs. Each worker owns one embedded Ansible
	// interpreter, so more workers allow more concurrent runs. Defaults to 1.
	Workers int `yaml:"workers,omitempty"`
	// Auth configures API authentication. Placeholder: parsed and validated
	// but not enforced yet.
	Auth AuthConfig `yaml:"auth,omitempty"`
	// Sandbox configures Landlock isolation of playbook runs. Disabled by
	// default: enabling it pins each worker to one collection and denies
	// Ansible writes outside a per-worker scratch directory.
	Sandbox SandboxConfig `yaml:"sandbox,omitempty"`
}

// SandboxConfig controls Landlock isolation between collections. It is
// opt-in because a sandboxed worker can only read its own collection and the
// system paths needed by Ansible/Python.
type SandboxConfig struct {
	// Enabled turns on per-collection workers and Landlock enforcement.
	Enabled bool `yaml:"enabled,omitempty"`
	// ReadOnly lists additional absolute paths made readable to every
	// sandboxed worker (for example a shared, read-only role repository).
	ReadOnly []string `yaml:"read_only,omitempty"`
	// Writable lists additional absolute paths a sandboxed worker may write
	// to. Collection directories stay read-only regardless of this list.
	Writable []string `yaml:"writable,omitempty"`
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

// Notification types accepted in collection.yml.
const (
	NotificationWebhook = "webhook"
	NotificationSlack   = "slack"
	NotificationEmail   = "email"
)

// NotificationSpec is the on-disk shape of one failure notifier of a target.
// Credentials belong in collection.yml (server-side), not in a git-backed
// target's strangle.yml.
type NotificationSpec struct {
	Type string `yaml:"type"`
	// Webhook and Slack notifiers POST to URL. Headers are extra HTTP headers
	// (useful for a bearer token or shared secret).
	URL     string            `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	// Email notifiers use a standard SMTP submission server.
	From         string   `yaml:"from,omitempty"`
	To           []string `yaml:"to,omitempty"`
	SMTP         string   `yaml:"smtp,omitempty"` // host:port
	Username     string   `yaml:"username,omitempty"`
	Password     string   `yaml:"password,omitempty"`
	PasswordFile string   `yaml:"password_file,omitempty"`
	// Timeout defaults to 10s.
	Timeout string `yaml:"timeout,omitempty"`
}

// NotificationConfig is a validated, resolved notifier.
type NotificationConfig struct {
	Type     string
	URL      string
	Headers  map[string]string
	From     string
	To       []string
	SMTP     string
	Username string
	Password string
	Timeout  time.Duration
}

// TargetSpec is one collection.yml entry. It points at the directory of an
// Ansible project, which must contain a strangle.yml with named playbooks.
type TargetSpec struct {
	// Name is the target's identifier within the collection. It is exposed
	// as the collection name in the versioned API.
	Name string `yaml:"name"`
	// Org is the organization the collection belongs to. It defaults to
	// "default" when omitted, so existing collection.yml files keep working.
	Org string `yaml:"org,omitempty"`
	// Path is the directory of the Ansible project (the directory that
	// contains strangle.yml).
	Path string `yaml:"path"`
	// Git describes the upstream repository of the project. When set,
	// strangle clones the repository into Path and keeps it in sync.
	Git GitConfig `yaml:"git,omitempty"`
	// Notifications sends a message when a run of this collection fails.
	Notifications []NotificationSpec `yaml:"notifications,omitempty"`
}

// GitConfig describes the upstream git repository of a target. When set,
// strangle clones the repository into the target's path and keeps it pinned
// to the tracked ref on every sync.
type GitConfig struct {
	// URL is the repository URL.
	URL string `yaml:"url,omitempty"`
	// Ref is a branch, tag or commit to track.
	Ref string `yaml:"ref,omitempty"`
	// Branch is the tracked branch; shorthand for Ref when Ref is empty.
	Branch string `yaml:"branch,omitempty"`
	// Token is an optional credential embedded in https clone/fetch URLs.
	Token string `yaml:"token,omitempty"`
}

// IsSet reports whether the target has an upstream repository configured.
func (g GitConfig) IsSet() bool { return g.URL != "" }

// EffectiveRef returns the ref the checkout is pinned to: Ref when set,
// otherwise Branch.
func (g GitConfig) EffectiveRef() string {
	if g.Ref != "" {
		return g.Ref
	}
	return g.Branch
}

// TargetConfig is one Ansible project: the collection.yml entry merged with
// the named playbooks of its directory's strangle.yml.
type TargetConfig struct {
	// Name is the target's identifier from the collection. It is exposed
	// as the collection name in the versioned API.
	Name string
	// Org is the organization grouping this collection with others. Empty
	// means the default organization ("default").
	Org string
	// Path is the resolved directory of the Ansible project.
	Path string
	// Git is the upstream repository placeholder from the collection.
	Git GitConfig
	// Notifications are the failure notifiers of this collection.
	Notifications []NotificationConfig

	// WorkingDir is the directory the playbooks run from. Ansible resolves
	// ansible.cfg, relative paths and roles relative to it. Defaults to
	// Path.
	WorkingDir string

	// Schedule is the target-level default for jobs that do not set their
	// own. Empty means jobs inherit server.yml's interval.
	Schedule string
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
	Schedule   string         `yaml:"schedule,omitempty"`
	Interval   string         `yaml:"interval,omitempty"`
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
	// Schedule is a duration or cron expression for this job. It overrides
	// the target-level schedule.
	Schedule string `yaml:"schedule,omitempty"`
	// Interval is a duration shorthand for Schedule.
	Interval string `yaml:"interval,omitempty"`
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
	// Schedule is the resolved duration or cron expression for this job.
	// Empty means it inherits server.yml's interval (and runs at startup,
	// like the original global cycle).
	Schedule string
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
// An optional first argument enables Ansible's check (dry-run) mode for one
// run without changing the collection configuration.
func (p PlaybookConfig) Args(check ...bool) []string {
	args := []string{"--diff", "-i", p.Inventory}
	if len(check) > 0 && check[0] {
		args = append(args, "--check")
	}

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
	// any target file is read. Relative notifier paths resolve against the
	// collection file, just like relative target paths.
	collDir := filepath.Dir(collectionPath)
	seen := map[string]struct{}{}
	for i, spec := range coll.Targets {
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
		if spec.Org == "" {
			spec.Org = DefaultOrg
		}
		if !validOrgName(spec.Org) {
			return Config{}, fmt.Errorf("collection: target %q: invalid org name %q", spec.Name, spec.Org)
		}
		coll.Targets[i].Org = spec.Org
		if _, err := resolveNotifications(spec.Notifications,
			fmt.Sprintf("collection: target %q", spec.Name), collDir); err != nil {
			return Config{}, err
		}
	}

	// Relative target paths resolve against the collection file's directory
	// (not the working directory), so a collection.yml can be placed next to
	// the targets it lists.
	cfg.Targets = make([]TargetConfig, 0, len(coll.Targets))
	for i := range coll.Targets {
		spec := coll.Targets[i]
		p := spec.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(collDir, p)
		}
		path, err := filepath.Abs(p)
		if err != nil {
			return Config{}, fmt.Errorf("collection: target %q: %w", spec.Name, err)
		}
		spec.Path = path
		if spec.Git.IsSet() {
			// A git-backed target's strangle.yml lives inside the repository
			// and may not exist before the first clone; the collection
			// manager loads it once the checkout is in place.
			notifications, err := resolveNotifications(spec.Notifications,
				fmt.Sprintf("collection: target %q", spec.Name), collDir)
			if err != nil {
				return Config{}, err
			}
			cfg.Targets = append(cfg.Targets, TargetConfig{
				Name:          spec.Name,
				Org:           spec.Org,
				Path:          path,
				Git:           spec.Git,
				Notifications: notifications,
				WorkingDir:    path,
			})
			continue
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

// LoadServer reads only the server.yml file, without a collection. It is
// used by commands that need server-level settings (like the DSN) without
// resolving the full target inventory.
func LoadServer(serverPath string) (ServerConfig, error) {
	var cfg ServerConfig
	data, err := os.ReadFile(serverPath)
	if err != nil {
		return ServerConfig{}, fmt.Errorf("read server config %q: %w", serverPath, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ServerConfig{}, fmt.Errorf("parse server config %q: %w", serverPath, err)
	}
	if err := cfg.validate(); err != nil {
		return ServerConfig{}, err
	}
	return cfg, nil
}

// loadTarget reads and validates the strangle.yml in the target directory.
func loadTarget(spec TargetSpec, path string) (TargetConfig, error) {
	return LoadTarget(spec, path, nil)
}

// LoadTarget reads and validates the strangle.yml in path and returns the
// resolved target. When vars is non-nil (git-backed targets, reloaded after a
// sync), its values are substituted as Go templates in the working_dir,
// playbook and inventory fields, so a branch can select different files, e.g.
// `inventory: inventories/{{ .Branch }}.ini`. Available variables: Name
// (the target name) and Branch (the tracked branch or ref).
func LoadTarget(spec TargetSpec, path string, vars map[string]string) (TargetConfig, error) {
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

	td := templateData{Name: spec.Name, Branch: vars["Branch"]}
	if wd, err := td.render(file.WorkingDir); err != nil {
		return TargetConfig{}, fmt.Errorf("target %q: working_dir: %w", spec.Name, err)
	} else {
		file.WorkingDir = wd
	}

	targetSchedule, err := resolveSchedule(file.Schedule, file.Interval, td, fmt.Sprintf("target %q", spec.Name))
	if err != nil {
		return TargetConfig{}, err
	}
	// Collection.yml owns the notifiers (and their credentials), not the
	// git-backed strangle.yml, so these are resolved from the spec rather than
	// the target file. Paths were made absolute during Load.
	notifications, err := resolveNotifications(spec.Notifications,
		fmt.Sprintf("target %q", spec.Name), path)
	if err != nil {
		return TargetConfig{}, err
	}

	org := spec.Org
	if org == "" {
		org = DefaultOrg
	}
	t := TargetConfig{
		Name:          spec.Name,
		Org:           org,
		Path:          path,
		Git:           spec.Git,
		Notifications: notifications,
		Schedule:      targetSchedule,
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
		if ps.Playbook, err = td.render(ps.Playbook); err != nil {
			return TargetConfig{}, fmt.Errorf("target %q: playbook %q: playbook: %w", spec.Name, ps.Name, err)
		}
		if ps.Inventory, err = td.render(ps.Inventory); err != nil {
			return TargetConfig{}, fmt.Errorf("target %q: playbook %q: inventory: %w", spec.Name, ps.Name, err)
		}
		if ps.Playbook == "" {
			return TargetConfig{}, fmt.Errorf("target %q: playbook %q needs a playbook file", spec.Name, ps.Name)
		}
		if ps.Inventory == "" {
			return TargetConfig{}, fmt.Errorf("target %q: playbook %q needs an inventory", spec.Name, ps.Name)
		}

		pbSchedule, err := resolveSchedule(ps.Schedule, ps.Interval, td,
			fmt.Sprintf("target %q: playbook %q", spec.Name, ps.Name))
		if err != nil {
			return TargetConfig{}, err
		}
		if pbSchedule == "" {
			pbSchedule = targetSchedule
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
			Schedule:   pbSchedule,
			WorkingDir: t.WorkingDir,
			Options:    options,
		})
	}
	return t, nil
}

// templateData holds the template variables available in strangle.yml fields
// of git-backed targets.
type templateData struct {
	Name   string
	Branch string
}

// render substitutes {{ .Name }} and {{ .Branch }} in s. Strings without
// template actions pass through unchanged.
func (d templateData) render(s string) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	tmpl, err := template.New("strangle").Parse(s)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// resolveSchedule normalizes a target or playbook level schedule. `schedule`
// accepts durations, cron expressions and aliases; `interval` is a duration
// shorthand for the common case. Either one may be templated in git-backed
// targets, so a branch can select a different cadence.
func resolveSchedule(scheduleSpec, interval string, td templateData, where string) (string, error) {
	if scheduleSpec != "" && interval != "" {
		return "", fmt.Errorf("%s: set either schedule or interval, not both", where)
	}
	raw := scheduleSpec
	if interval != "" {
		rendered, err := td.render(interval)
		if err != nil {
			return "", fmt.Errorf("%s: interval: %w", where, err)
		}
		d, err := time.ParseDuration(rendered)
		if err != nil || d <= 0 {
			return "", fmt.Errorf("%s: interval %q must be a positive duration", where, interval)
		}
		raw = rendered
	} else if raw != "" {
		rendered, err := td.render(raw)
		if err != nil {
			return "", fmt.Errorf("%s: schedule: %w", where, err)
		}
		raw = rendered
	}
	if raw == "" {
		return "", nil
	}
	if _, err := sched.Parse(raw); err != nil {
		return "", fmt.Errorf("%s: %w", where, err)
	}
	return raw, nil
}

// resolveNotifications validates and resolves the failure notifiers declared
// on a collection.yml target. Relative password_file paths are rewritten to
// absolute paths in specs, so a later reload (after a git sync) resolves the
// same credentials even though it no longer knows where collection.yml lived.
func resolveNotifications(specs []NotificationSpec, where, baseDir string) ([]NotificationConfig, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]NotificationConfig, 0, len(specs))
	for i := range specs {
		spec := &specs[i]
		label := fmt.Sprintf("%s: notifications[%d]", where, i)
		typ := strings.ToLower(strings.TrimSpace(spec.Type))
		n := NotificationConfig{Type: typ}

		timeout := 10 * time.Second
		if spec.Timeout != "" {
			d, err := time.ParseDuration(spec.Timeout)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("%s: timeout %q must be a positive duration", label, spec.Timeout)
			}
			timeout = d
		}
		n.Timeout = timeout

		switch typ {
		case NotificationWebhook, NotificationSlack:
			if spec.URL == "" {
				return nil, fmt.Errorf("%s: %s requires a url", label, typ)
			}
			u, err := url.Parse(spec.URL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return nil, fmt.Errorf("%s: url %q must be an absolute http(s) URL", label, spec.URL)
			}
			n.URL = spec.URL
			for k, v := range spec.Headers {
				if k == "" || strings.ContainsAny(k, ":\r\n") || strings.ContainsAny(v, "\r\n") {
					return nil, fmt.Errorf("%s: invalid header %q", label, k)
				}
			}
			n.Headers = make(map[string]string, len(spec.Headers))
			for k, v := range spec.Headers {
				n.Headers[k] = v
			}
		case NotificationEmail:
			from, err := parseMailAddress(spec.From)
			if err != nil {
				return nil, fmt.Errorf("%s: from: %w", label, err)
			}
			if len(spec.To) == 0 {
				return nil, fmt.Errorf("%s: email requires at least one `to` address", label)
			}
			n.To = make([]string, 0, len(spec.To))
			for _, to := range spec.To {
				addr, err := parseMailAddress(to)
				if err != nil {
					return nil, fmt.Errorf("%s: to: %w", label, err)
				}
				n.To = append(n.To, addr)
			}
			n.From = from
			if spec.SMTP == "" {
				return nil, fmt.Errorf("%s: email requires an smtp host:port", label)
			}
			host, port, err := net.SplitHostPort(spec.SMTP)
			if err != nil || host == "" {
				return nil, fmt.Errorf("%s: smtp %q must be host:port", label, spec.SMTP)
			}
			portNum, err := strconv.Atoi(port)
			if err != nil || portNum < 1 || portNum > 65535 {
				return nil, fmt.Errorf("%s: smtp %q has an invalid port", label, spec.SMTP)
			}
			n.SMTP = spec.SMTP
			if spec.Password != "" && spec.PasswordFile != "" {
				return nil, fmt.Errorf("%s: set either password or password_file, not both", label)
			}
			if spec.PasswordFile != "" {
				p := spec.PasswordFile
				if !filepath.IsAbs(p) {
					p = filepath.Join(baseDir, p)
					spec.PasswordFile = p
				}
				data, err := os.ReadFile(p)
				if err != nil {
					return nil, fmt.Errorf("%s: read password_file %q: %w", label, p, err)
				}
				n.Password = strings.TrimRight(string(data), "\r\n")
			} else {
				n.Password = spec.Password
			}
			n.Username = spec.Username
			if n.Password != "" && n.Username == "" {
				return nil, fmt.Errorf("%s: password requires a username", label)
			}
		default:
			return nil, fmt.Errorf("%s: type must be one of %s, %s, %s (got %q)",
				label, NotificationWebhook, NotificationEmail, NotificationSlack, typ)
		}
		out = append(out, n)
	}
	return out, nil
}

func parseMailAddress(value string) (string, error) {
	addr, err := mail.ParseAddress(value)
	if err != nil || addr.Address != value {
		return "", fmt.Errorf("invalid email address %q", value)
	}
	return addr.Address, nil
}

// validOrgName rejects path separators and other characters that would make
// an org name unsafe as a URL path segment.
func validOrgName(name string) bool {
	return validPlaybookName(name)
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
	if s.Workers <= 0 {
		s.Workers = 1
	}
	return nil
}
