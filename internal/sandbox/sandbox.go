// Package sandbox builds and applies Landlock policies for playbook workers.
//
// A sandboxed worker is pinned to one collection: it may read that
// collection's files and the system paths Ansible/Python need, but not its
// siblings' files or credentials. Ansible writes go to a private scratch
// directory; the collection checkout stays read-only.
//
// Landlock is Linux-only and depends on kernel support. A policy is applied
// with BestEffort, so an older kernel enforces as much of it as it can. On a
// kernel without Landlock at all the worker still runs, but without
// isolation; callers can report that through KernelSupported().
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kakwa/strangle/internal/config"
	"github.com/landlock-lsm/go-landlock/landlock"
)

// Rule modes. "ro" grants read/execute, "rw" grants read and write.
const (
	ModeRO = "ro"
	ModeRW = "rw"
)

// Rule kinds. Landlock needs different rules for file hierarchies and
// directories; the builder records the kind so Apply does not have to stat
// paths after the worker has already been restricted.
const (
	KindDir  = "dir"
	KindFile = "file"
)

// Rule is one path allowed by a sandbox. ResolveUnix is needed for pathname
// unix sockets such as the worker's gRPC socket. Refer allows renames and
// links between directories inside the scratch tree.
type Rule struct {
	Path          string `json:"path"`
	Mode          string `json:"mode"`
	Kind          string `json:"kind"`
	IgnoreMissing bool   `json:"ignore_missing,omitempty"`
	ResolveUnix   bool   `json:"resolve_unix,omitempty"`
	Refer         bool   `json:"refer,omitempty"`
}

// Manifest is the policy handed to a worker at startup. The master writes it
// to a file in its private temp directory; the worker reads it before
// applying Landlock.
type Manifest struct {
	Collection string            `json:"collection"`
	Rules      []Rule            `json:"rules"`
	Env        map[string]string `json:"env,omitempty"`
}

// ReadFile loads a manifest written by the master.
func ReadFile(path string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("read sandbox manifest %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("parse sandbox manifest %q: %w", path, err)
	}
	if m.Collection == "" {
		return m, fmt.Errorf("sandbox manifest %q has no collection", path)
	}
	return m, nil
}

// WriteFile writes a manifest for the worker process.
func (m Manifest) WriteFile(path string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// SetEnv applies the environment a sandbox needs before SSH managers and the
// embedded Python interpreter are initialized. Ansible's temporary files,
// cache and HOME are redirected into the worker's private scratch directory.
func SetEnv(env map[string]string) {
	for k, v := range env {
		_ = os.Setenv(k, v)
	}
}

// KernelSupported reports whether the running kernel lists landlock in its
// LSMs. It is used only to warn when a requested sandbox is not enforced.
func KernelSupported() bool {
	data, err := os.ReadFile("/sys/kernel/security/lsm")
	return err == nil && strings.Contains(string(data), "landlock")
}

// Apply restricts the current process to m using Landlock. It must be called
// once, before any playbook runs; Landlock restrictions cannot be undone.
func Apply(m Manifest) error {
	rules := make([]landlock.Rule, 0, len(m.Rules))
	for _, r := range m.Rules {
		if r.IgnoreMissing {
			if _, err := os.Stat(r.Path); errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		var rule landlock.FSRule
		switch r.Mode {
		case ModeRO:
			if r.Kind == KindDir {
				rule = landlock.RODirs(r.Path)
			} else {
				rule = landlock.ROFiles(r.Path)
			}
		case ModeRW:
			if r.Kind == KindDir {
				rule = landlock.RWDirs(r.Path)
			} else {
				rule = landlock.RWFiles(r.Path)
			}
		default:
			return fmt.Errorf("unknown sandbox rule mode %q for %q", r.Mode, r.Path)
		}
		if r.ResolveUnix {
			rule = rule.WithResolveUnix()
		}
		if r.Refer {
			rule = rule.WithRefer()
		}
		rules = append(rules, rule)
	}
	return landlock.V5.BestEffort().RestrictPaths(rules...)
}

// Build creates one base manifest per collection. The master adds a scratch
// directory and socket rules for each worker slot before writing them out.
func Build(cfg config.SandboxConfig, targets []config.TargetConfig, serverPath, collectionPath string) (map[string]Manifest, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	out := make(map[string]Manifest, len(targets))
	serverAbs, err := filepath.Abs(serverPath)
	if err != nil {
		return nil, err
	}
	collAbs, err := filepath.Abs(collectionPath)
	if err != nil {
		return nil, err
	}
	system := systemPaths()

	for _, t := range targets {
		m := Manifest{Collection: t.Name}
		seen := map[string]bool{}
		add := func(r Rule) {
			key := r.Path + "\x00" + r.Mode + "\x00" + r.Kind
			if !seen[key] {
				seen[key] = true
				m.Rules = append(m.Rules, r)
			}
		}
		addRO := func(path string) {
			p, kind, err := normalize(path, KindFile)
			if err != nil {
				return
			}
			add(Rule{Path: p, Mode: ModeRO, Kind: kind, IgnoreMissing: true})
		}
		addRW := func(path string) {
			p, kind, err := normalize(path, KindFile)
			if err != nil || pathInsideAny(p, t.Path, t.WorkingDir) {
				// Never grant write access to the collection itself: that is
				// the whole point of the sandbox.
				return
			}
			add(Rule{Path: p, Mode: ModeRW, Kind: kind})
		}

		for _, p := range system {
			addRO(p)
		}
		addRO(serverAbs)
		addRO(collAbs)
		addRO(t.Path)
		addRO(t.WorkingDir)
		for _, p := range cfg.ReadOnly {
			addRO(p)
		}
		for _, p := range cfg.Writable {
			addRW(p)
		}

		// Host-key trust files are managed by strangle, not by Ansible.
		// They live in the collection but must stay writable by the worker.
		for _, pb := range t.Playbooks {
			if !pb.Options.SSH.Enabled {
				continue
			}
			// Credential and certificate material can live outside the
			// collection; workers need read access to sign with it.
			addRO(pb.Options.SSH.PrivateKeyFile)
			if cert := pb.Options.SSH.Certificate; cert != nil {
				for _, p := range []string{
					cert.Internal.CAKeyFile,
					cert.Vault.CACertFile,
					cert.Vault.RoleIDFile,
					cert.Vault.SecretIDFile,
				} {
					addRO(p)
				}
			}
			for _, p := range []string{pb.Options.SSH.AutoAcceptFile, pb.Options.SSH.PendingFile} {
				if p == "" {
					continue
				}
				if err := ensureFile(p); err != nil {
					return nil, fmt.Errorf("collection %q: prepare host-key file %q: %w", t.Name, p, err)
				}
				abs, kind, err := normalize(p, KindFile)
				if err != nil {
					return nil, err
				}
				add(Rule{Path: abs, Mode: ModeRW, Kind: kind})
			}
		}
		out[t.Name] = m
	}
	return out, nil
}

// systemPaths are the hierarchies needed by the Go/Python/Ansible runtime.
// They are shared, read-only and never include user collections.
func systemPaths() []string {
	candidates := []string{
		"/usr", "/bin", "/sbin", "/lib", "/lib64", "/etc", "/proc", "/dev", "/opt",
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	out := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, p := range candidates {
		if _, err := os.Stat(p); err != nil || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func normalize(path, fallbackKind string) (string, string, error) {
	if path == "" {
		return "", "", errors.New("empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	abs = filepath.Clean(abs)
	kind := fallbackKind
	if st, err := os.Stat(abs); err == nil {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if st.IsDir() {
			kind = KindDir
		}
	}
	return abs, kind, nil
}

func pathInsideAny(path string, roots ...string) bool {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		rel, err := filepath.Rel(filepath.Clean(abs), path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ensureFile creates an empty host-key database so a Landlock rule can be
// attached to the inode before the worker starts.
func ensureFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(`{"approved":{},"pending":{}}`), 0o600)
}
