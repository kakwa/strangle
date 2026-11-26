package sshpool

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/kakwa/strangle/utils/cfg"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Sentinel errors for host key trust decisions; callers (the API) map them
// to HTTP statuses with errors.Is.
var (
	// ErrNoPendingKey is returned when approving or rejecting a host that
	// has no pending key.
	ErrNoPendingKey = errors.New("no pending host key")
	// ErrFingerprintMismatch is returned when the given fingerprint does not
	// match the pending key.
	ErrFingerprintMismatch = errors.New("fingerprint mismatch")
)

// HostKey describes a key seen for a host.
type HostKey struct {
	// Target is the scope key (<target>/<playbook name>) that owns the
	// connection. It is set when host keys are aggregated across scopes (see Multi);
	// the per-scope persistence files leave it empty.
	Target      string    `json:"target,omitempty"`
	Host        string    `json:"host"`
	Fingerprint string    `json:"fingerprint"`
	PublicKey   string    `json:"public_key"`
	Status      string    `json:"status"`
	FirstSeen   time.Time `json:"first_seen"`
}

type keyDatabase struct {
	Approved map[string]HostKey `json:"approved"`
	Pending  map[string]HostKey `json:"pending"`
}

// HostKeyStore persists host key trust decisions and builds SSH callbacks.
type HostKeyStore struct {
	mu       sync.Mutex
	policy   cfg.HostKeyPolicy
	conf     cfg.SSHConfig
	db       keyDatabase
	caKeys   []ssh.PublicKey
	callback ssh.HostKeyCallback
}

// NewHostKeyStore creates a store from configuration.
func NewHostKeyStore(conf cfg.SSHConfig) (*HostKeyStore, error) {
	policy := conf.HostKeyPolicy
	if policy == "" {
		policy = cfg.PolicyAPI
	}

	s := &HostKeyStore{
		policy: policy,
		conf:   conf,
		db: keyDatabase{
			Approved: map[string]HostKey{},
			Pending:  map[string]HostKey{},
		},
	}

	switch policy {
	case cfg.PolicyKnownHosts:
		cb, err := knownhosts.New(conf.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("load known_hosts %q: %w", conf.KnownHostsFile, err)
		}
		s.callback = cb
	case cfg.PolicyCA:
		for _, path := range conf.CAFiles {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read CA file %q: %w", path, err)
			}
			key, _, _, _, err := ssh.ParseAuthorizedKey(data)
			if err != nil {
				return nil, fmt.Errorf("parse CA file %q: %w", path, err)
			}
			s.caKeys = append(s.caKeys, key)
		}
		if len(s.caKeys) == 0 {
			return nil, errors.New("host_key_policy ca requires at least one ca_files entry")
		}
		checker := &ssh.CertChecker{
			IsHostAuthority: func(auth ssh.PublicKey, _ string) bool {
				for _, ca := range s.caKeys {
					if bytes.Equal(auth.Marshal(), ca.Marshal()) {
						return true
					}
				}
				return false
			},
		}
		s.callback = checker.CheckHostKey
	case cfg.PolicyAPI, cfg.PolicyAuto:
		if err := s.load(); err != nil {
			return nil, err
		}
		s.callback = s.verify
	case cfg.PolicyInsecure:
		s.callback = ssh.InsecureIgnoreHostKey()
	default:
		return nil, fmt.Errorf("unknown host_key_policy %q", policy)
	}

	return s, nil
}

// Callback returns the ssh.HostKeyCallback for this store.
func (s *HostKeyStore) Callback() ssh.HostKeyCallback {
	return s.callback
}

func (s *HostKeyStore) verify(host string, _ net.Addr, key ssh.PublicKey) error {
	host = stripPort(host)
	fingerprint := ssh.FingerprintSHA256(key)
	record := HostKey{
		Host:        host,
		Fingerprint: fingerprint,
		PublicKey:   string(ssh.MarshalAuthorizedKey(key)),
		Status:      "approved",
		FirstSeen:   time.Now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Reload from disk: the trust decision may have been made in another
	// process (the master or a worker) since this store was constructed.
	if err := s.reloadLocked(); err != nil {
		return err
	}

	known, ok := s.db.Approved[host]
	if !ok {
		if s.policy == cfg.PolicyAPI {
			record.Status = "pending"
			s.db.Pending[host] = record
			_ = s.save()
			return fmt.Errorf("host key %s for %s is pending approval", fingerprint, host)
		}
		s.db.Approved[host] = record
		_ = s.save()
		return nil
	}

	if known.Fingerprint != fingerprint {
		err := fmt.Errorf("host key for %s changed: expected %s, got %s", host, known.Fingerprint, fingerprint)
		if s.policy == cfg.PolicyAPI {
			record.Status = "rejected"
			s.db.Pending[host] = record
			_ = s.save()
		}
		return err
	}

	return nil
}

// reloadLocked re-reads the trust files from disk. Decisions can be made by
// another process (master and workers share the files), so file-backed
// policies refresh before reading them. The caller must hold s.mu.
func (s *HostKeyStore) reloadLocked() error {
	if s.policy == cfg.PolicyAPI || s.policy == cfg.PolicyAuto {
		return s.load()
	}
	return nil
}

// Pending lists host keys waiting for approval. It re-reads the pending file
// so keys created by another process (a worker) are visible.
func (s *HostKeyStore) Pending() []HostKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	result := make([]HostKey, 0, len(s.db.Pending))
	for _, key := range s.db.Pending {
		result = append(result, key)
	}
	return result
}

// Approve trusts a pending host key.
func (s *HostKeyStore) Approve(host, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.reloadLocked(); err != nil {
		return err
	}

	key, ok := s.db.Pending[host]
	if !ok {
		return fmt.Errorf("%w for %s", ErrNoPendingKey, host)
	}
	if fingerprint != "" && key.Fingerprint != fingerprint {
		return fmt.Errorf("%w for %s", ErrFingerprintMismatch, host)
	}

	key.Status = "approved"
	s.db.Approved[host] = key
	delete(s.db.Pending, host)
	return s.save()
}

// Reject removes a pending host key.
func (s *HostKeyStore) Reject(host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.reloadLocked(); err != nil {
		return err
	}

	if _, ok := s.db.Pending[host]; !ok {
		return fmt.Errorf("%w for %s", ErrNoPendingKey, host)
	}
	delete(s.db.Pending, host)
	return s.save()
}

func (s *HostKeyStore) load() error {
	approved, err := loadKeys(s.conf.AutoAcceptFile)
	if err != nil {
		return err
	}
	pending, err := loadKeys(s.conf.PendingFile)
	if err != nil {
		return err
	}
	s.db.Approved = approved
	s.db.Pending = pending
	return nil
}

func loadKeys(path string) (map[string]HostKey, error) {
	keys := map[string]HostKey{}
	if path == "" {
		return keys, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return keys, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read host key store %q: %w", path, err)
	}
	if len(data) == 0 {
		return keys, nil
	}
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("parse host key store %q: %w", path, err)
	}
	if keys == nil {
		keys = map[string]HostKey{}
	}
	return keys, nil
}

func (s *HostKeyStore) save() error {
	if err := saveKeys(s.conf.AutoAcceptFile, s.db.Approved); err != nil {
		return err
	}
	if s.conf.PendingFile != "" {
		return saveKeys(s.conf.PendingFile, s.db.Pending)
	}
	return nil
}

func saveKeys(path string, keys map[string]HostKey) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// stripPort removes a trailing :port from an SSH callback host, which may
// arrive either with or without one depending on the dial form.
func stripPort(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}

// EncodeKey is used by the bridge to pass public keys as JSON strings.
func EncodeKey(key ssh.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key.Marshal())
}
