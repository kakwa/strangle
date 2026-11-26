package sshpool

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kakwa/strangle/utils/cfg"
	"golang.org/x/crypto/ssh"
)

func testSignerFile(t *testing.T, name string) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return signer, path
}

func certificateFromSigner(t *testing.T, signer ssh.Signer) *ssh.Certificate {
	t.Helper()
	cert, ok := signer.PublicKey().(*ssh.Certificate)
	if !ok {
		t.Fatalf("signer public key = %T, want *ssh.Certificate", signer.PublicKey())
	}
	return cert
}

func TestInternalCAProvider(t *testing.T) {
	_, subjectPath := testSignerFile(t, "id_ed25519")
	authority, caPath := testSignerFile(t, "ca_ed25519")
	conf := cfg.SSHConfig{
		PrivateKeyFile: subjectPath,
		Certificate: &cfg.CertificateConfig{
			Backend:     cfg.CertificateInternal,
			TTL:         time.Minute,
			Principals:  []string{"deploy"},
			KeyIDPrefix: "unit",
			Internal:    cfg.InternalCAConfig{CAKeyFile: caPath},
		},
	}
	provider, err := newSignerProvider(conf, nil)
	if err != nil {
		t.Fatalf("newSignerProvider: %v", err)
	}
	signers, refreshAt, err := provider.Signers(Endpoint{User: "ubuntu"})
	if err != nil {
		t.Fatalf("Signers: %v", err)
	}
	if len(signers) != 1 || !refreshAt.After(time.Now()) {
		t.Fatalf("Signers = %d signers, refresh at %v", len(signers), refreshAt)
	}
	cert := certificateFromSigner(t, signers[0])
	if cert.CertType != ssh.UserCert || !strings.Contains(cert.KeyId, "unit-") {
		t.Errorf("certificate = %+v, want a user cert with a prefixed key ID", cert)
	}
	if !certPrincipalsContains(cert.ValidPrincipals, "ubuntu") || !certPrincipalsContains(cert.ValidPrincipals, "deploy") {
		t.Errorf("principals = %v, want the endpoint user and configured principals", cert.ValidPrincipals)
	}
	if got := cert.ValidBefore - cert.ValidAfter; got > 150 {
		t.Errorf("certificate validity = %ds, want about one minute plus clock skew", got)
	}
	checker := &ssh.CertChecker{IsUserAuthority: func(key ssh.PublicKey) bool {
		return bytes.Equal(key.Marshal(), authority.PublicKey().Marshal())
	}}
	if err := checker.CheckCert("ubuntu", cert); err != nil {
		t.Errorf("certificate does not verify against the CA: %v", err)
	}

	// A second call reuses the cached signer until the refresh deadline.
	cached, _, err := provider.Signers(Endpoint{User: "ubuntu"})
	if err != nil {
		t.Fatalf("cached Signers: %v", err)
	}
	if cached[0] != signers[0] {
		t.Error("certificate was reissued before its refresh deadline")
	}
	internal := provider.(*internalCAProvider)
	internal.refreshAt = time.Now().Add(-time.Second)
	refreshed, _, err := provider.Signers(Endpoint{User: "ubuntu"})
	if err != nil {
		t.Fatalf("refreshed Signers: %v", err)
	}
	if refreshed[0] == signers[0] {
		t.Error("certificate was not reissued after its refresh deadline")
	}
}

func certPrincipalsContains(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

func TestVaultProvider(t *testing.T) {
	_, subjectPath := testSignerFile(t, "id_ed25519")
	authority, _ := testSignerFile(t, "vault_ca_ed25519")
	var logins, signs int
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/approle/login":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("login body: %v", err)
			}
			if body["role_id"] != "role" || body["secret_id"] != "secret" {
				t.Errorf("login credentials = %v", body)
			}
			mu.Lock()
			logins++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"auth":{"client_token":"vault-token","lease_duration":3600}}`))
		case "/v1/ssh-lab/sign/strangle":
			if got := r.Header.Get("X-Vault-Token"); got != "vault-token" {
				t.Errorf("X-Vault-Token = %q", got)
			}
			if got := r.Header.Get("X-Vault-Namespace"); got != "lab" {
				t.Errorf("X-Vault-Namespace = %q", got)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("sign body: %v", err)
			}
			if body["ttl"] != "2m0s" || body["valid_principals"] != "ubuntu,deploy" {
				t.Errorf("sign request = %v", body)
			}
			key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(body["public_key"]))
			if err != nil {
				t.Errorf("public_key: %v", err)
			}
			now := time.Now()
			cert := &ssh.Certificate{
				Key:             key,
				CertType:        ssh.UserCert,
				KeyId:           body["key_id"],
				ValidPrincipals: []string{"deploy", "ubuntu"},
				ValidAfter:      uint64(now.Add(-time.Minute).Unix()),
				ValidBefore:     uint64(now.Add(2 * time.Minute).Unix()),
			}
			if err := cert.SignCert(rand.Reader, authority); err != nil {
				t.Errorf("sign cert: %v", err)
			}
			mu.Lock()
			signs++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]string{"signed_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(cert)))},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	conf := cfg.SSHConfig{
		PrivateKeyFile: subjectPath,
		Certificate: &cfg.CertificateConfig{
			Backend:     cfg.CertificateVault,
			TTL:         2 * time.Minute,
			Principals:  []string{"deploy"},
			KeyIDPrefix: "vault-test",
			Vault: cfg.VaultCertificateConfig{
				Addr:      server.URL,
				Namespace: "lab",
				Mount:     "ssh-lab",
				Role:      "strangle",
				RoleID:    "role",
				SecretID:  "secret",
			},
		},
	}
	provider, err := newSignerProvider(conf, nil)
	if err != nil {
		t.Fatalf("newSignerProvider: %v", err)
	}
	signers, _, err := provider.Signers(Endpoint{User: "ubuntu"})
	if err != nil {
		t.Fatalf("Signers: %v", err)
	}
	cert := certificateFromSigner(t, signers[0])
	checker := &ssh.CertChecker{IsUserAuthority: func(key ssh.PublicKey) bool {
		return bytes.Equal(key.Marshal(), authority.PublicKey().Marshal())
	}}
	if err := checker.CheckCert("ubuntu", cert); err != nil {
		t.Errorf("Vault certificate does not verify: %v", err)
	}
	cached, _, err := provider.Signers(Endpoint{User: "ubuntu"})
	if err != nil || cached[0] != signers[0] {
		t.Fatalf("cached Signers = %v, %v", cached, err)
	}

	vault := provider.(*vaultProvider)
	vault.refreshAt = time.Now().Add(-time.Second)
	if _, _, err := provider.Signers(Endpoint{User: "ubuntu"}); err != nil {
		t.Fatalf("refreshed Signers: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if logins != 1 || signs != 2 {
		t.Errorf("Vault calls = %d logins, %d signs; want 1 and 2", logins, signs)
	}
}

func TestManagerExpiryAndEndpointNormalization(t *testing.T) {
	host, port := startTestSSHServer(t)
	signer, _ := testSignerFile(t, "id_ed25519")
	provider := &fakeProvider{signers: []ssh.Signer{signer}, expires: time.Now().Add(time.Hour)}
	hostKeys, err := NewHostKeyStore(cfg.SSHConfig{HostKeyPolicy: cfg.PolicyInsecure})
	if err != nil {
		t.Fatalf("NewHostKeyStore: %v", err)
	}
	m := &Manager{
		conf:       cfg.SSHConfig{User: "deploy", Port: port},
		provider:   provider,
		clients:    map[string]*ssh.Client{},
		retired:    map[string][]*ssh.Client{},
		authExpiry: map[string]time.Time{},
		inUse:      map[string]int{},
		hostKeys:   hostKeys,
	}
	defer m.Close()

	first, err := m.client(Endpoint{Host: host})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	// An explicitly specified endpoint reuses the same pooled connection.
	second, err := m.client(Endpoint{Host: host, User: "deploy", Port: port})
	if err != nil {
		t.Fatalf("client with explicit endpoint: %v", err)
	}
	if first != second || m.Stats().TotalDials != 1 {
		t.Errorf("dials = %d, want one reused connection", m.Stats().TotalDials)
	}

	key := "deploy@" + net.JoinHostPort(host, strconv.Itoa(port))
	m.mu.Lock()
	m.authExpiry[key] = time.Now().Add(-time.Second)
	m.mu.Unlock()
	third, err := m.client(Endpoint{Host: host})
	if err != nil {
		t.Fatalf("client after expiry: %v", err)
	}
	if third == first || m.Stats().TotalDials != 2 {
		t.Errorf("dials = %d, want a fresh connection after expiry", m.Stats().TotalDials)
	}

	// drop() applies the same defaults as client(), so a bare endpoint still
	// evicts the normalized cache entry.
	m.drop(Endpoint{Host: host})
	if got := m.Stats().ActiveConnections; got != 0 {
		t.Errorf("active connections after drop = %d, want 0", got)
	}
}

func TestExpiryKeepsInFlightClientAlive(t *testing.T) {
	host, port := startTestSSHServer(t)
	signer, _ := testSignerFile(t, "id_ed25519")
	hostKeys, err := NewHostKeyStore(cfg.SSHConfig{HostKeyPolicy: cfg.PolicyInsecure})
	if err != nil {
		t.Fatalf("NewHostKeyStore: %v", err)
	}
	m := &Manager{
		conf:       cfg.SSHConfig{User: "deploy", Port: port},
		provider:   &staticSignerProvider{signers: []ssh.Signer{signer}},
		clients:    map[string]*ssh.Client{},
		retired:    map[string][]*ssh.Client{},
		authExpiry: map[string]time.Time{},
		inUse:      map[string]int{},
		hostKeys:   hostKeys,
	}
	defer m.Close()

	inFlight, key, err := m.acquire(Endpoint{Host: host})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	m.mu.Lock()
	m.authExpiry[key] = time.Now().Add(-time.Second)
	m.mu.Unlock()

	fresh, err := m.client(Endpoint{Host: host})
	if err != nil {
		t.Fatalf("client after expiry: %v", err)
	}
	if fresh == inFlight || m.Stats().TotalDials != 2 {
		t.Fatalf("dials = %d, want a fresh connection for new work", m.Stats().TotalDials)
	}
	session, err := inFlight.NewSession()
	if err != nil {
		t.Fatalf("in-flight connection was closed under an active task: %v", err)
	}
	_ = session.Close()
	m.release(key)
}

type fakeProvider struct {
	mu      sync.Mutex
	signers []ssh.Signer
	expires time.Time
	calls   int
}

func (p *fakeProvider) Signers(Endpoint) ([]ssh.Signer, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.signers, p.expires, nil
}

func (p *fakeProvider) Close() error { return nil }

func startTestSSHServer(t *testing.T) (string, int) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}
	conf := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	conf.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveTestSSH(conn, conf)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return host, port
}

func serveTestSSH(conn net.Conn, conf *ssh.ServerConfig) {
	serverConn, channels, requests, err := ssh.NewServerConn(conn, conf)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer serverConn.Close()
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.Prohibited, "test server")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range channelRequests {
				_ = req.Reply(req.Type == "exec" || req.Type == "subsystem", nil)
			}
		}()
		go func() {
			_, _ = io.Copy(io.Discard, channel)
			_ = channel.Close()
		}()
	}
}
