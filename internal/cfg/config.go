// Package config loads strangle's runtime configuration from three kinds of
// YAML files:
//
//   - server.yml: process-level settings (listen address, log level, default
//     run schedule, database, auth placeholder).
//   - collection.yml: the inventory of Ansible projects (targets) that
//     strangle applies; a target may carry a git upstream that strangle
//     clones and keeps in sync.
//   - strangle.yml: one per target, at the root of the project directory.
//     Each entry of its `playbooks` list is a named job with its own
//     playbook file, inventory, cron schedule and Ansible/SSH options.
//
// Configuration is file-only: there are no environment variables.
package cfg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/robfig/cron/v3"
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
	// Schedule is the fallback schedule for jobs that do not set one in
	// strangle.yml, in robfig/cron v3 format. Defaults to @every 15m.
	Schedule string `yaml:"schedule,omitempty"`
	// Database configures the run-history database.
	Database DatabaseConfig `yaml:"database,omitempty"`
	// Workers is how many worker child processes the master spawns to
	// execute playbook jobs. Each worker owns one embedded Ansible
	// interpreter, so more workers allow more concurrent runs. Defaults to 1.
	Workers int `yaml:"workers,omitempty"`
	// Auth configures API authentication. Token is the static sync token;
	// CLIToken is a user bearer token used by the CLI subcommands.
	Auth AuthConfig `yaml:"auth,omitempty"`
	// Sandbox configures Landlock isolation of playbook runs. Disabled by
	// default: enabling it pins each worker to one collection and denies
	// Ansible writes outside a per-worker scratch directory.
	Sandbox SandboxConfig `yaml:"sandbox,omitempty"`
	// Retention is the default run-history policy for collections that do
	// not declare their own. Disabled when both limits are zero.
	Retention RetentionConfig `yaml:"retention,omitempty"`
	// HostCleanup is the default policy for removing hosts that are no
	// longer in a collection's inventory and have not been seen for a while.
	HostCleanup HostCleanupConfig `yaml:"host_cleanup,omitempty"`
	// TLS enables HTTPS for the HTTP server. Both files must be set.
	TLS TLSConfig `yaml:"tls,omitempty"`
}

// TLSConfig points at the certificate and private key used by the HTTP
// server. Both paths must be set; leaving them empty keeps plain HTTP.
type TLSConfig struct {
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}

// HostCleanupConfig controls automatic host removal. Delay is the minimum
// age of the host's last sight before it may be removed; Interval is the
// maintenance cadence and is only read from server.yml. Disabled when Delay
// is zero.
type HostCleanupConfig struct {
	Interval time.Duration `yaml:"interval,omitempty"`
	Delay    time.Duration `yaml:"delay,omitempty"`
}

// Enabled reports whether automatic host cleanup is configured.
func (h HostCleanupConfig) Enabled() bool { return h.Delay > 0 }

// RetentionConfig purges finished run history. MaxAge removes runs older
// than the duration; MaxCount keeps only the newest N runs. Both limits are
// optional and can be combined: a run is purged when either rule selects it.
// Interval is the maintenance cadence and is only read from server.yml.
type RetentionConfig struct {
	Interval time.Duration `yaml:"interval,omitempty"`
	MaxAge   time.Duration `yaml:"max_age,omitempty"`
	MaxCount int           `yaml:"max_count,omitempty"`
}

// Enabled reports whether either retention rule is configured.
func (r RetentionConfig) Enabled() bool {
	return r.MaxAge > 0 || r.MaxCount > 0
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

// AuthConfig holds the two independent credentials read from server.yml.
type AuthConfig struct {
	// Token is the static sync token. It is accepted only by POST /api/sync
	// and is never treated as a user identity.
	Token string `yaml:"token,omitempty"`
	// CLIToken is a user API token (st_...) used by strangle-cli commands.
	// The server ignores it: it is a client-side setting only.
	CLIToken string `yaml:"cli_token,omitempty"`
}

// DatabaseConfig configures the PostgreSQL database that stores run history.
type DatabaseConfig struct {
	// DSN is the PostgreSQL connection string. It is required: run history,
	// users and the job queue all live in the same database.
	DSN string `yaml:"dsn"`
}

// CollectionConfig is the content of collection.yml: the inventory of
// Ansible projects (collections) that strangle applies.
type CollectionConfig struct {
	// Collections is the ordered list of projects to apply.
	Collections []TargetSpec `yaml:"collections"`
}

// UnmarshalYAML accepts the current `collections:` list and the legacy
// `targets:` list used by older collection.yml files. The two are mutually
// exclusive; specifying both is a configuration error.
func (c *CollectionConfig) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		Collections []TargetSpec `yaml:"collections"`
		Targets     []TargetSpec `yaml:"targets"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	if len(raw.Collections) > 0 && len(raw.Targets) > 0 {
		return errors.New("collection: use either collections or targets, not both")
	}
	c.Collections = raw.Collections
	if len(c.Collections) == 0 {
		c.Collections = raw.Targets
	}
	return nil
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
	// Retention overrides the server's default run-history policy for this
	// collection. Only MaxAge and MaxCount are used here; the maintenance
	// interval is process-wide.
	Retention RetentionConfig `yaml:"retention,omitempty"`
	// HostCleanup overrides the server's host-cleanup delay for this
	// collection. Only Delay is used here.
	HostCleanup HostCleanupConfig `yaml:"host_cleanup,omitempty"`
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
	// Retention is this collection's run-history policy, overriding the
	// server default when it enables either rule.
	Retention RetentionConfig
	// HostCleanup overrides the server's host-cleanup delay when enabled.
	HostCleanup HostCleanupConfig

	// WorkingDir is the directory the playbooks run from. Ansible resolves
	// ansible.cfg, relative paths and roles relative to it. Defaults to
	// Path.
	WorkingDir string

	// Schedule is the target-level default for jobs that do not set their
	// own. Empty means jobs inherit server.yml's schedule.
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
	// Schedule is a robfig/cron v3 expression for this job. It overrides
	// the target-level schedule.
	Schedule string `yaml:"schedule,omitempty"`
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
	Enabled              *bool            `yaml:"enabled,omitempty"`
	User                 string           `yaml:"user,omitempty"`
	Port                 *int             `yaml:"port,omitempty"`
	PrivateKeyFile       string           `yaml:"private_key_file,omitempty"`
	PrivateKeyPassphrase string           `yaml:"private_key_passphrase,omitempty"`
	UseAgent             *bool            `yaml:"use_agent,omitempty"`
	Password             string           `yaml:"password,omitempty"`
	HostKeyPolicy        HostKeyPolicy    `yaml:"host_key_policy,omitempty"`
	KnownHostsFile       string           `yaml:"known_hosts_file,omitempty"`
	CAFiles              []string         `yaml:"ca_files,omitempty"`
	AutoAcceptFile       string           `yaml:"auto_accept_file,omitempty"`
	PendingFile          string           `yaml:"pending_file,omitempty"`
	Certificate          *CertificateSpec `yaml:"certificate,omitempty"`
}

// CertificateSpec configures short-lived SSH user certificates. It is a
// pointer in SSHSpec so a playbook override can distinguish "unset" from an
// explicit backend choice.
type CertificateSpec struct {
	Backend     CertificateBackend    `yaml:"backend,omitempty"`
	TTL         string                `yaml:"ttl,omitempty"`
	Principals  []string              `yaml:"principals,omitempty"`
	KeyIDPrefix string                `yaml:"key_id_prefix,omitempty"`
	Internal    *InternalCASpec       `yaml:"internal,omitempty"`
	Vault       *VaultCertificateSpec `yaml:"vault,omitempty"`
}

// InternalCASpec configures signing with a local CA private key.
type InternalCASpec struct {
	CAKeyFile string `yaml:"ca_key_file,omitempty"`
}

// VaultCertificateSpec configures Vault's SSH secrets engine. Credentials can
// come from YAML or, preferably, from files (e.g. mounted AppRole secrets).
type VaultCertificateSpec struct {
	Addr         string `yaml:"addr,omitempty"`
	Namespace    string `yaml:"namespace,omitempty"`
	Mount        string `yaml:"mount,omitempty"`
	Role         string `yaml:"role,omitempty"`
	AuthMount    string `yaml:"auth_mount,omitempty"`
	RoleID       string `yaml:"role_id,omitempty"`
	RoleIDFile   string `yaml:"role_id_file,omitempty"`
	SecretID     string `yaml:"secret_id,omitempty"`
	SecretIDFile string `yaml:"secret_id_file,omitempty"`
	CACertFile   string `yaml:"ca_cert_file,omitempty"`
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
	// Schedule is the resolved robfig/cron v3 expression for this job.
	// Empty means it inherits server.yml's schedule.
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

// CertificateBackend selects how ephemeral SSH user certificates are signed.
type CertificateBackend string

const (
	// CertificateInternal signs certificates with a local CA private key.
	CertificateInternal CertificateBackend = "internal"
	// CertificateVault signs certificates through Vault's SSH secrets engine.
	CertificateVault CertificateBackend = "vault"
)

// Valid reports whether b is a supported certificate backend.
func (b CertificateBackend) Valid() bool {
	return b == CertificateInternal || b == CertificateVault
}

// CertificateConfig is the resolved certificate configuration of one playbook.
type CertificateConfig struct {
	Backend     CertificateBackend
	TTL         time.Duration
	Principals  []string
	KeyIDPrefix string
	Internal    InternalCAConfig
	Vault       VaultCertificateConfig
}

// InternalCAConfig is the resolved local-CA certificate configuration.
type InternalCAConfig struct {
	CAKeyFile string
}

// VaultCertificateConfig is the resolved Vault certificate configuration.
// RoleID and SecretID may have been read from files at config load time.
type VaultCertificateConfig struct {
	Addr         string
	Namespace    string
	Mount        string
	Role         string
	AuthMount    string
	RoleID       string
	RoleIDFile   string
	SecretID     string
	SecretIDFile string
	CACertFile   string
}

// SSHConfig configures the persistent SSH connections owned by the Go process
// for one playbook.
type SSHConfig struct {
	Enabled              bool               `yaml:"enabled,omitempty"`
	User                 string             `yaml:"user,omitempty"`
	Port                 int                `yaml:"port,omitempty"`
	PrivateKeyFile       string             `yaml:"private_key_file,omitempty"`
	PrivateKeyPassphrase string             `yaml:"private_key_passphrase,omitempty"`
	UseAgent             bool               `yaml:"use_agent,omitempty"`
	Password             string             `yaml:"password,omitempty"`
	HostKeyPolicy        HostKeyPolicy      `yaml:"host_key_policy,omitempty"`
	KnownHostsFile       string             `yaml:"known_hosts_file,omitempty"`
	CAFiles              []string           `yaml:"ca_files,omitempty"`
	AutoAcceptFile       string             `yaml:"auto_accept_file,omitempty"`
	PendingFile          string             `yaml:"pending_file,omitempty"`
	Certificate          *CertificateConfig `yaml:"certificate,omitempty"`
}

// readServer and readCollection are the shared read/parse steps of the three
// public loaders. They do not validate: LoadServer validates server settings
// and LoadCollection only needs the target/org mapping.
func readServer(path string) (ServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ServerConfig{}, fmt.Errorf("read server config %q: %w", path, err)
	}
	var cfg ServerConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return ServerConfig{}, fmt.Errorf("parse server config %q: %w", path, err)
	}
	return cfg, nil
}

func readCollection(path string) (CollectionConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CollectionConfig{}, fmt.Errorf("read collection %q: %w", path, err)
	}
	var coll CollectionConfig
	if err := yaml.Unmarshal(data, &coll); err != nil {
		return CollectionConfig{}, fmt.Errorf("parse collection %q: %w", path, err)
	}
	if len(coll.Collections) == 0 {
		return CollectionConfig{}, fmt.Errorf("collection %q: collections is required (non-empty list)", path)
	}
	return coll, nil
}

// normalizeTargetSpecs checks the fields every loader needs and fills in the
// default org. Path and policy validation are done only by Load.
func normalizeTargetSpecs(coll *CollectionConfig) error {
	seen := map[string]struct{}{}
	for i := range coll.Targets {
		spec := &coll.Targets[i]
		if spec.Name == "" {
			return errors.New("collection: every target needs a name")
		}
		if _, dup := seen[spec.Name]; dup {
			return fmt.Errorf("collection: duplicate target name %q", spec.Name)
		}
		seen[spec.Name] = struct{}{}
		if spec.Org == "" {
			spec.Org = DefaultOrg
		}
		if !validOrgName(spec.Org) {
			return fmt.Errorf("collection: target %q: invalid org name %q", spec.Name, spec.Org)
		}
	}
	return nil
}

// positiveDuration parses a config duration that must be strictly positive.
// display is the original (possibly untemplated) value shown in errors.
func positiveDuration(raw, label, display string) (time.Duration, error) {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s %q must be a positive duration", label, display)
	}
	return d, nil
}

// Load reads the server.yml and collection.yml files and resolves each
// target's strangle.yml, applying defaults.
func Load(serverPath, collectionPath string) (Config, error) {
	cfg := Config{}
	var err error
	if cfg.Server, err = readServer(serverPath); err != nil {
		return Config{}, err
	}
	coll, err := readCollection(collectionPath)
	if err != nil {
		return Config{}, err
	}
	if err := normalizeTargetSpecs(&coll); err != nil {
		return Config{}, err
	}

	collDir := filepath.Dir(collectionPath)
	for i, spec := range coll.Targets {
		label := fmt.Sprintf("collection: target %q", spec.Name)
		if spec.Path == "" {
			return Config{}, fmt.Errorf("%s needs a path", label)
		}
		if err := validateRetention(label, &spec.Retention); err != nil {
			return Config{}, err
		}
		coll.Targets[i].Retention = spec.Retention
		if err := validateHostCleanup(label, &spec.HostCleanup); err != nil {
			return Config{}, err
		}
		coll.Targets[i].HostCleanup = spec.HostCleanup
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
			cfg.Targets = append(cfg.Targets, TargetConfig{
				Name:        spec.Name,
				Org:         spec.Org,
				Path:        path,
				Git:         spec.Git,
				Retention:   spec.Retention,
				HostCleanup: spec.HostCleanup,
				WorkingDir:  path,
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
	cfg, err := readServer(serverPath)
	if err != nil {
		return ServerConfig{}, err
	}
	if err := cfg.validate(); err != nil {
		return ServerConfig{}, err
	}
	return cfg, nil
}

// LoadCollection reads only collection.yml and applies the org defaults. It
// is used by client-side commands that need the target -> org mapping but do
// not want to resolve every target's strangle.yml.
func LoadCollection(collectionPath string) ([]TargetSpec, error) {
	coll, err := readCollection(collectionPath)
	if err != nil {
		return nil, err
	}
	if err := normalizeTargetSpecs(&coll); err != nil {
		return nil, err
	}
	return coll.Targets, nil
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

	targetSchedule, err := resolveSchedule(file.Schedule, td, fmt.Sprintf("target %q", spec.Name))
	if err != nil {
		return TargetConfig{}, err
	}
	org := spec.Org
	if org == "" {
		org = DefaultOrg
	}
	t := TargetConfig{
		Name:        spec.Name,
		Org:         org,
		Path:        path,
		Git:         spec.Git,
		Retention:   spec.Retention,
		HostCleanup: spec.HostCleanup,
		Schedule:    targetSchedule,
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

		pbSchedule, err := resolveSchedule(ps.Schedule, td,
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

// resolveSchedule validates a target or playbook level robfig/cron v3
// schedule. It may be templated in git-backed targets, so a branch can select a
// different cadence.
func resolveSchedule(scheduleSpec string, td templateData, where string) (string, error) {
	raw, err := td.render(scheduleSpec)
	if err != nil {
		return "", fmt.Errorf("%s: schedule: %w", where, err)
	}
	if raw == "" {
		return "", nil
	}
	parsed, err := cron.ParseStandard(raw)
	if err != nil {
		return "", fmt.Errorf("%s: invalid schedule %q: %w", where, raw, err)
	}
	if parsed.Next(time.Now()).IsZero() {
		return "", fmt.Errorf("%s: schedule %q has no upcoming occurrence", where, raw)
	}
	return raw, nil
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
	if over.Certificate != nil {
		if base.Certificate == nil {
			out.Certificate = over.Certificate
		} else {
			merged := mergeCertificate(*base.Certificate, *over.Certificate)
			out.Certificate = &merged
		}
	}
	return out
}

// mergeCertificate overlays a playbook's certificate options on the target
// defaults. Nested backend blocks are merged field by field as well.
func mergeCertificate(base, over CertificateSpec) CertificateSpec {
	out := base
	if over.Backend != "" {
		out.Backend = over.Backend
	}
	if over.TTL != "" {
		out.TTL = over.TTL
	}
	if len(over.Principals) > 0 {
		out.Principals = over.Principals
	}
	if over.KeyIDPrefix != "" {
		out.KeyIDPrefix = over.KeyIDPrefix
	}
	if over.Internal != nil {
		if base.Internal == nil {
			out.Internal = over.Internal
		} else {
			merged := *base.Internal
			if over.Internal.CAKeyFile != "" {
				merged.CAKeyFile = over.Internal.CAKeyFile
			}
			out.Internal = &merged
		}
	}
	if over.Vault != nil {
		if base.Vault == nil {
			out.Vault = over.Vault
		} else {
			merged := *base.Vault
			overlayString(&merged.Addr, over.Vault.Addr)
			overlayString(&merged.Namespace, over.Vault.Namespace)
			overlayString(&merged.Mount, over.Vault.Mount)
			overlayString(&merged.Role, over.Vault.Role)
			overlayString(&merged.AuthMount, over.Vault.AuthMount)
			overlayString(&merged.RoleID, over.Vault.RoleID)
			overlayString(&merged.RoleIDFile, over.Vault.RoleIDFile)
			overlayString(&merged.SecretID, over.Vault.SecretID)
			overlayString(&merged.SecretIDFile, over.Vault.SecretIDFile)
			overlayString(&merged.CACertFile, over.Vault.CACertFile)
			out.Vault = &merged
		}
	}
	return out
}

func overlayString(dst *string, value string) {
	if value != "" {
		*dst = value
	}
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
	if s.Certificate != nil {
		cert, err := resolveCertificate(*s.Certificate)
		if err != nil {
			return o, fmt.Errorf("ssh.certificate: %w", err)
		}
		o.SSH.Certificate = &cert
	}
	if o.SSH.Certificate != nil {
		if o.SSH.PrivateKeyFile == "" {
			return o, errors.New("ssh.certificate requires ssh.private_key_file")
		}
		if o.SSH.UseAgent {
			return o, errors.New("ssh.certificate does not support ssh.use_agent; use ssh.private_key_file")
		}
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

// resolveCertificate validates certificate options and fills in defaults.
func resolveCertificate(spec CertificateSpec) (CertificateConfig, error) {
	c := CertificateConfig{
		Backend:     spec.Backend,
		TTL:         10 * time.Minute,
		Principals:  append([]string(nil), spec.Principals...),
		KeyIDPrefix: spec.KeyIDPrefix,
	}
	if !c.Backend.Valid() {
		return c, fmt.Errorf("backend must be %q or %q (got %q)", CertificateInternal, CertificateVault, c.Backend)
	}
	if spec.TTL != "" {
		ttl, err := time.ParseDuration(spec.TTL)
		if err != nil {
			return c, fmt.Errorf("ttl %q is not a duration: %w", spec.TTL, err)
		}
		if ttl < time.Minute || ttl > 24*time.Hour {
			return c, fmt.Errorf("ttl %q must be between 1m and 24h", spec.TTL)
		}
		c.TTL = ttl
	}
	if c.KeyIDPrefix == "" {
		c.KeyIDPrefix = "strangle"
	}

	switch c.Backend {
	case CertificateInternal:
		if spec.Internal == nil || spec.Internal.CAKeyFile == "" {
			return c, errors.New("internal requires internal.ca_key_file")
		}
		c.Internal = InternalCAConfig{CAKeyFile: spec.Internal.CAKeyFile}
	case CertificateVault:
		if spec.Vault == nil {
			return c, errors.New("vault requires a vault block")
		}
		v := spec.Vault
		u, err := url.Parse(v.Addr)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return c, fmt.Errorf("vault.addr %q must be an http(s) URL", v.Addr)
		}
		if v.Role == "" {
			return c, errors.New("vault.role is required")
		}
		if (v.RoleID == "" && v.RoleIDFile == "") || (v.SecretID == "" && v.SecretIDFile == "") {
			return c, errors.New("vault requires role_id/secret_id or role_id_file/secret_id_file")
		}
		c.Vault = VaultCertificateConfig{
			Addr:         v.Addr,
			Namespace:    v.Namespace,
			Mount:        v.Mount,
			Role:         v.Role,
			AuthMount:    v.AuthMount,
			RoleID:       v.RoleID,
			RoleIDFile:   v.RoleIDFile,
			SecretID:     v.SecretID,
			SecretIDFile: v.SecretIDFile,
			CACertFile:   v.CACertFile,
		}
		if c.Vault.Mount == "" {
			c.Vault.Mount = "ssh"
		}
		if c.Vault.AuthMount == "" {
			c.Vault.AuthMount = "approle"
		}
	}
	return c, nil
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
	if strings.TrimSpace(s.Schedule) == "" {
		s.Schedule = "@every 15m"
	}
	parsed, err := cron.ParseStandard(s.Schedule)
	if err != nil {
		return fmt.Errorf("server: schedule: invalid schedule %q: %w", s.Schedule, err)
	}
	if parsed.Next(time.Now()).IsZero() {
		return fmt.Errorf("server: schedule %q has no upcoming occurrence", s.Schedule)
	}
	if strings.TrimSpace(s.Database.DSN) == "" {
		return errors.New("server: database.dsn is required")
	}
	if s.Workers <= 0 {
		s.Workers = 1
	}
	if err := validateRetention("server", &s.Retention); err != nil {
		return err
	}
	if err := validateHostCleanup("server", &s.HostCleanup); err != nil {
		return err
	}
	if (s.TLS.CertFile == "") != (s.TLS.KeyFile == "") {
		return errors.New("server: tls.cert_file and tls.key_file must be set together")
	}
	return nil
}

// validateRetention rejects negative limits and gives an enabled policy a
// default maintenance interval.
func validateRetention(where string, r *RetentionConfig) error {
	if r.Interval < 0 {
		return fmt.Errorf("%s: retention.interval must not be negative", where)
	}
	if r.MaxAge < 0 {
		return fmt.Errorf("%s: retention.max_age must not be negative", where)
	}
	if r.MaxCount < 0 {
		return fmt.Errorf("%s: retention.max_count must not be negative", where)
	}
	if r.Enabled() && r.Interval == 0 {
		r.Interval = time.Hour
	}
	return nil
}

// validateHostCleanup rejects negative delays and gives an enabled policy a
// default maintenance interval.
func validateHostCleanup(where string, h *HostCleanupConfig) error {
	if h.Interval < 0 {
		return fmt.Errorf("%s: host_cleanup.interval must not be negative", where)
	}
	if h.Delay < 0 {
		return fmt.Errorf("%s: host_cleanup.delay must not be negative", where)
	}
	if h.Enabled() && h.Interval == 0 {
		h.Interval = time.Hour
	}
	return nil
}
