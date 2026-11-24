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

	"github.com/kakwa/strangle/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

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

// HostKey describes a key seen for a host.
type HostKey struct {
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
	policy   HostKeyPolicy
	cfg      config.SSHConfig
	db       keyDatabase
	caKeys   []ssh.PublicKey
	callback ssh.HostKeyCallback
}

// NewHostKeyStore creates a store from configuration.
func NewHostKeyStore(cfg config.SSHConfig) (*HostKeyStore, error) {
	policy := HostKeyPolicy(cfg.HostKeyPolicy)
	if policy == "" {
		policy = PolicyAPI
	}

	s := &HostKeyStore{
		policy: policy,
		cfg:    cfg,
		db: keyDatabase{
			Approved: map[string]HostKey{},
			Pending:  map[string]HostKey{},
		},
	}

	switch policy {
	case PolicyKnownHosts:
		cb, err := knownhosts.New(cfg.KnownHostsFile)
		if err != nil {
			return nil, fmt.Errorf("load known_hosts %q: %w", cfg.KnownHostsFile, err)
		}
		s.callback = cb
	case PolicyCA:
		for _, path := range cfg.CAFiles {
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
					if bytesEqual(auth.Marshal(), ca.Marshal()) {
						return true
					}
				}
				return false
			},
		}
		s.callback = checker.CheckHostKey
	case PolicyAPI, PolicyAuto:
		if err := s.load(); err != nil {
			return nil, err
		}
		s.callback = s.verify
	case PolicyInsecure:
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
	host, _, err := net.SplitHostPort(host)
	if err != nil {
		host = stripPort(host)
	}

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

	known, ok := s.db.Approved[host]
	if !ok {
		if s.policy == PolicyAPI {
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
		if s.policy == PolicyAPI {
			record.Status = "rejected"
			s.db.Pending[host] = record
			_ = s.save()
		}
		return err
	}

	return nil
}

// Pending lists host keys waiting for approval.
func (s *HostKeyStore) Pending() []HostKey {
	s.mu.Lock()
	defer s.mu.Unlock()
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

	key, ok := s.db.Pending[host]
	if !ok {
		return fmt.Errorf("no pending host key for %s", host)
	}
	if fingerprint != "" && key.Fingerprint != fingerprint {
		return fmt.Errorf("fingerprint mismatch for %s", host)
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

	if _, ok := s.db.Pending[host]; !ok {
		return fmt.Errorf("no pending host key for %s", host)
	}
	delete(s.db.Pending, host)
	return s.save()
}

func (s *HostKeyStore) load() error {
	approved, err := loadKeys(s.cfg.AutoAcceptFile)
	if err != nil {
		return err
	}
	pending, err := loadKeys(s.cfg.PendingFile)
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
	if err := saveKeys(s.cfg.AutoAcceptFile, s.db.Approved); err != nil {
		return err
	}
	if s.cfg.PendingFile != "" {
		return saveKeys(s.cfg.PendingFile, s.db.Pending)
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

func stripPort(hostport string) string {
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		return host
	}
	return hostport
}

func bytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}

// EncodeKey is used by the bridge to pass public keys as JSON strings.
func EncodeKey(key ssh.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key.Marshal())
}
