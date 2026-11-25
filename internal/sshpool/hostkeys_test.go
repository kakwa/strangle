package sshpool

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kakwa/strangle/internal/cfg"
	"golang.org/x/crypto/ssh"
)

func testKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	key, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatalf("new public key: %v", err)
	}
	return key
}

func testAddr(host string) net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22, Zone: host}
}

func TestHostKeyStoreAPI(t *testing.T) {
	dir := t.TempDir()
	conf := cfg.SSHConfig{
		HostKeyPolicy:  "api",
		AutoAcceptFile: filepath.Join(dir, "accepted.json"),
		PendingFile:    filepath.Join(dir, "pending.json"),
	}

	store, err := NewHostKeyStore(conf)
	if err != nil {
		t.Fatalf("NewHostKeyStore: %v", err)
	}
	key := testKey(t)
	fingerprint := ssh.FingerprintSHA256(key)

	// First connection: key is pending.
	if err := store.Callback()("example.com:22", testAddr("example.com"), key); err == nil {
		t.Fatal("first connection succeeded, want pending error")
	}
	pending := store.Pending()
	if len(pending) != 1 || pending[0].Fingerprint != fingerprint {
		t.Fatalf("Pending() = %v, want 1 key with fingerprint %s", pending, fingerprint)
	}

	// Approve and reconnect.
	if err := store.Approve("example.com", fingerprint); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := store.Callback()("example.com:22", testAddr("example.com"), key); err != nil {
		t.Fatalf("approved key rejected: %v", err)
	}
	if len(store.Pending()) != 0 {
		t.Errorf("Pending() after approve = %v, want empty", store.Pending())
	}

	// Persisted across restarts.
	reloaded, err := NewHostKeyStore(conf)
	if err != nil {
		t.Fatalf("NewHostKeyStore (reload): %v", err)
	}
	if err := reloaded.Callback()("example.com:22", testAddr("example.com"), key); err != nil {
		t.Fatalf("persisted key rejected: %v", err)
	}

	// Changed key is rejected.
	other := testKey(t)
	err = reloaded.Callback()("example.com:22", testAddr("example.com"), other)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed key error = %v, want 'changed'", err)
	}
}

func TestHostKeyStoreAuto(t *testing.T) {
	dir := t.TempDir()
	conf := cfg.SSHConfig{
		HostKeyPolicy:  "auto",
		AutoAcceptFile: filepath.Join(dir, "accepted.json"),
		PendingFile:    filepath.Join(dir, "pending.json"),
	}

	store, err := NewHostKeyStore(conf)
	if err != nil {
		t.Fatalf("NewHostKeyStore: %v", err)
	}
	key := testKey(t)

	if err := store.Callback()("host:22", testAddr("host"), key); err != nil {
		t.Fatalf("first key rejected: %v", err)
	}
	if err := store.Callback()("host:22", testAddr("host"), key); err != nil {
		t.Fatalf("same key rejected: %v", err)
	}
	if len(store.Pending()) != 0 {
		t.Errorf("auto policy created pending keys: %v", store.Pending())
	}
}

func TestHostKeyStoreReject(t *testing.T) {
	dir := t.TempDir()
	conf := cfg.SSHConfig{
		HostKeyPolicy:  "api",
		AutoAcceptFile: filepath.Join(dir, "accepted.json"),
		PendingFile:    filepath.Join(dir, "pending.json"),
	}
	store, err := NewHostKeyStore(conf)
	if err != nil {
		t.Fatalf("NewHostKeyStore: %v", err)
	}
	_ = store.Callback()("host:22", testAddr("host"), testKey(t))

	if err := store.Reject("host"); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if len(store.Pending()) != 0 {
		t.Errorf("Pending() after reject = %v, want empty", store.Pending())
	}
}

func TestHostKeyStorePolicies(t *testing.T) {
	for _, policy := range []cfg.HostKeyPolicy{"api", "auto", "insecure"} {
		conf := cfg.SSHConfig{HostKeyPolicy: policy, AutoAcceptFile: filepath.Join(t.TempDir(), "keys.json")}
		if _, err := NewHostKeyStore(conf); err != nil {
			t.Errorf("policy %q: %v", policy, err)
		}
	}

	// CA policy without any CA files is an error.
	conf := cfg.SSHConfig{HostKeyPolicy: "ca"}
	if _, err := NewHostKeyStore(conf); err == nil {
		t.Error("ca policy without ca_files succeeded, want error")
	}

	// Unknown policy is an error.
	conf.HostKeyPolicy = "bogus"
	if _, err := NewHostKeyStore(conf); err == nil {
		t.Error("unknown policy succeeded, want error")
	}
}

func TestEncodeKey(t *testing.T) {
	key := testKey(t)
	encoded := EncodeKey(key)
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		t.Fatalf("EncodeKey produced invalid base64: %v", err)
	}
}

func TestManagerStats(t *testing.T) {
	m := &Manager{clients: map[string]*ssh.Client{}}
	stats := m.Stats()
	if stats.ActiveConnections != 0 || stats.TotalDials != 0 {
		t.Errorf("Stats() = %+v, want zeros", stats)
	}

	var nilManager *Manager
	if stats := nilManager.Stats(); stats != (Stats{}) {
		t.Errorf("nil manager Stats() = %+v, want zeros", stats)
	}
}

func TestLoadSigner(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(pemBytes), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	signer, err := loadSigner(path, "")
	if err != nil {
		t.Fatalf("loadSigner: %v", err)
	}
	if signer == nil {
		t.Fatal("loadSigner returned nil signer")
	}
}
