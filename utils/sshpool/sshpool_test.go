package sshpool

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSshpool(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Sshpool Suite")
}

func testSignerFile(name string) (ssh.Signer, string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(GinkgoT().TempDir(), name)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, "", err
	}
	return signer, path, nil
}

func certificateFromSigner(signer ssh.Signer) (*ssh.Certificate, error) {
	cert, ok := signer.PublicKey().(*ssh.Certificate)
	if !ok {
		return nil, fmt.Errorf("signer public key = %T, want *ssh.Certificate", signer.PublicKey())
	}
	return cert, nil
}

func testKey() (ssh.PublicKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewPublicKey(priv.Public())
}

func testAddr(host string) net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22, Zone: host}
}

func certPrincipalsContains(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
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

func startTestSSHServer() (string, int, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", 0, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return "", 0, err
	}
	conf := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	conf.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", 0, err
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
	DeferCleanup(func() {
		_ = listener.Close()
		<-done
	})

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
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

var _ = Describe("InternalCAProvider", func() {
	It("signs a user certificate with the internal CA and refreshes after the deadline", func() {
		_, subjectPath, err := testSignerFile("id_ed25519")
		Expect(err).NotTo(HaveOccurred())
		authority, caPath, err := testSignerFile("ca_ed25519")
		Expect(err).NotTo(HaveOccurred())
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
		Expect(err).NotTo(HaveOccurred(), "newSignerProvider")
		signers, refreshAt, err := provider.Signers(Endpoint{User: "ubuntu"})
		Expect(err).NotTo(HaveOccurred(), "Signers")
		Expect(signers).To(HaveLen(1))
		Expect(refreshAt).To(BeTemporally(">", time.Now()))
		cert, err := certificateFromSigner(signers[0])
		Expect(err).NotTo(HaveOccurred(), "certificateFromSigner")
		Expect(cert.CertType).To(Equal(ssh.UserCert), "certificate = %+v, want a user cert with a prefixed key ID", cert)
		Expect(cert.KeyId).To(ContainSubstring("unit-"), "certificate = %+v, want a user cert with a prefixed key ID", cert)
		Expect(cert.ValidPrincipals).To(ContainElements("ubuntu", "deploy"),
			"principals = %v, want the endpoint user and configured principals", cert.ValidPrincipals)
		Expect(int64(cert.ValidBefore - cert.ValidAfter)).To(BeNumerically("<=", 150),
			"certificate validity = %ds, want about one minute plus clock skew", cert.ValidBefore-cert.ValidAfter)
		checker := &ssh.CertChecker{IsUserAuthority: func(key ssh.PublicKey) bool {
			return bytes.Equal(key.Marshal(), authority.PublicKey().Marshal())
		}}
		Expect(checker.CheckCert("ubuntu", cert)).NotTo(HaveOccurred(), "certificate does not verify against the CA")

		// A second call reuses the cached signer until the refresh deadline.
		cached, _, err := provider.Signers(Endpoint{User: "ubuntu"})
		Expect(err).NotTo(HaveOccurred(), "cached Signers")
		Expect(cached[0]).To(Equal(signers[0]), "certificate was reissued before its refresh deadline")
		internal := provider.(*internalCAProvider)
		internal.refreshAt = time.Now().Add(-time.Second)
		refreshed, _, err := provider.Signers(Endpoint{User: "ubuntu"})
		Expect(err).NotTo(HaveOccurred(), "refreshed Signers")
		Expect(refreshed[0]).ToNot(Equal(signers[0]), "certificate was not reissued after its refresh deadline")
	})
})

var _ = Describe("VaultProvider", func() {
	It("authenticates with AppRole, signs certificates through Vault and refreshes after the deadline", func() {
		_, subjectPath, err := testSignerFile("id_ed25519")
		Expect(err).NotTo(HaveOccurred())
		authority, _, err := testSignerFile("vault_ca_ed25519")
		Expect(err).NotTo(HaveOccurred())
		var logins, signs int
		var mu sync.Mutex

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/v1/auth/approle/login":
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					GinkgoT().Errorf("login body: %v", err)
				}
				if body["role_id"] != "role" || body["secret_id"] != "secret" {
					GinkgoT().Errorf("login credentials = %v", body)
				}
				mu.Lock()
				logins++
				mu.Unlock()
				_, _ = w.Write([]byte(`{"auth":{"client_token":"vault-token","lease_duration":3600}}`))
			case r.URL.Path == "/v1/ssh-lab/sign/strangle":
				if got := r.Header.Get("X-Vault-Token"); got != "vault-token" {
					GinkgoT().Errorf("X-Vault-Token = %q", got)
				}
				if got := r.Header.Get("X-Vault-Namespace"); got != "lab" {
					GinkgoT().Errorf("X-Vault-Namespace = %q", got)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					GinkgoT().Errorf("sign body: %v", err)
				}
				if body["ttl"] != "2m0s" || body["valid_principals"] != "ubuntu,deploy" {
					GinkgoT().Errorf("sign request = %v", body)
				}
				key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(body["public_key"]))
				if err != nil {
					GinkgoT().Errorf("public_key: %v", err)
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
					GinkgoT().Errorf("sign cert: %v", err)
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
		Expect(err).NotTo(HaveOccurred(), "newSignerProvider")
		signers, _, err := provider.Signers(Endpoint{User: "ubuntu"})
		Expect(err).NotTo(HaveOccurred(), "Signers")
		cert, err := certificateFromSigner(signers[0])
		Expect(err).NotTo(HaveOccurred(), "certificateFromSigner")
		checker := &ssh.CertChecker{IsUserAuthority: func(key ssh.PublicKey) bool {
			return bytes.Equal(key.Marshal(), authority.PublicKey().Marshal())
		}}
		Expect(checker.CheckCert("ubuntu", cert)).NotTo(HaveOccurred(), "Vault certificate does not verify")
		cached, _, err := provider.Signers(Endpoint{User: "ubuntu"})
		Expect(err).NotTo(HaveOccurred(), "cached Signers")
		Expect(cached[0]).To(Equal(signers[0]), "cached Signers = %v, %v", cached, err)

		vault := provider.(*vaultProvider)
		vault.refreshAt = time.Now().Add(-time.Second)
		_, _, err = provider.Signers(Endpoint{User: "ubuntu"})
		Expect(err).NotTo(HaveOccurred(), "refreshed Signers")
		mu.Lock()
		defer mu.Unlock()
		Expect(logins).To(Equal(1), "Vault calls = %d logins, %d signs; want 1 and 2", logins, signs)
		Expect(signs).To(Equal(2), "Vault calls = %d logins, %d signs; want 1 and 2", logins, signs)
	})
})

var _ = Describe("ManagerExpiryAndEndpointNormalization", func() {
	It("reuses pooled connections, replaces expired ones and evicts bare endpoints", func() {
		host, port, err := startTestSSHServer()
		Expect(err).NotTo(HaveOccurred())
		signer, _, err := testSignerFile("id_ed25519")
		Expect(err).NotTo(HaveOccurred())
		provider := &fakeProvider{signers: []ssh.Signer{signer}, expires: time.Now().Add(time.Hour)}
		hostKeys, err := NewHostKeyStore(cfg.SSHConfig{HostKeyPolicy: cfg.PolicyInsecure})
		Expect(err).NotTo(HaveOccurred(), "NewHostKeyStore")
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
		Expect(err).NotTo(HaveOccurred(), "client")
		// An explicitly specified endpoint reuses the same pooled connection.
		second, err := m.client(Endpoint{Host: host, User: "deploy", Port: port})
		Expect(err).NotTo(HaveOccurred(), "client with explicit endpoint")
		Expect(second).To(Equal(first), "dials = %d, want one reused connection", m.Stats().TotalDials)
		Expect(m.Stats().TotalDials).To(Equal(1), "dials = %d, want one reused connection", m.Stats().TotalDials)

		key := "deploy@" + net.JoinHostPort(host, strconv.Itoa(port))
		m.mu.Lock()
		m.authExpiry[key] = time.Now().Add(-time.Second)
		m.mu.Unlock()
		third, err := m.client(Endpoint{Host: host})
		Expect(err).NotTo(HaveOccurred(), "client after expiry")
		Expect(third).ToNot(Equal(first), "dials = %d, want a fresh connection after expiry", m.Stats().TotalDials)
		Expect(m.Stats().TotalDials).To(Equal(2), "dials = %d, want a fresh connection after expiry", m.Stats().TotalDials)

		// drop() applies the same defaults as client(), so a bare endpoint still
		// evicts the normalized cache entry.
		m.drop(Endpoint{Host: host})
		Expect(m.Stats().ActiveConnections).To(Equal(0), "active connections after drop = %d, want 0", m.Stats().ActiveConnections)
	})
})

var _ = Describe("ExpiryKeepsInFlightClientAlive", func() {
	It("keeps an in-flight client alive after its auth expiry", func() {
		host, port, err := startTestSSHServer()
		Expect(err).NotTo(HaveOccurred())
		signer, _, err := testSignerFile("id_ed25519")
		Expect(err).NotTo(HaveOccurred())
		hostKeys, err := NewHostKeyStore(cfg.SSHConfig{HostKeyPolicy: cfg.PolicyInsecure})
		Expect(err).NotTo(HaveOccurred(), "NewHostKeyStore")
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
		Expect(err).NotTo(HaveOccurred(), "acquire")
		m.mu.Lock()
		m.authExpiry[key] = time.Now().Add(-time.Second)
		m.mu.Unlock()

		fresh, err := m.client(Endpoint{Host: host})
		Expect(err).NotTo(HaveOccurred(), "client after expiry")
		Expect(fresh).ToNot(Equal(inFlight), "dials = %d, want a fresh connection for new work", m.Stats().TotalDials)
		Expect(m.Stats().TotalDials).To(Equal(2), "dials = %d, want a fresh connection for new work", m.Stats().TotalDials)
		session, err := inFlight.NewSession()
		Expect(err).NotTo(HaveOccurred(), "in-flight connection was closed under an active task")
		_ = session.Close()
		m.release(key)
	})
})

var _ = Describe("HostKeyStoreAPI", func() {
	It("keeps unknown keys pending, approves them and rejects changed keys", func() {
		dir := GinkgoT().TempDir()
		conf := cfg.SSHConfig{
			HostKeyPolicy:  "api",
			AutoAcceptFile: filepath.Join(dir, "accepted.json"),
			PendingFile:    filepath.Join(dir, "pending.json"),
		}

		store, err := NewHostKeyStore(conf)
		Expect(err).NotTo(HaveOccurred(), "NewHostKeyStore")
		key, err := testKey()
		Expect(err).NotTo(HaveOccurred())
		fingerprint := ssh.FingerprintSHA256(key)

		// First connection: key is pending.
		Expect(store.Callback()("example.com:22", testAddr("example.com"), key)).To(HaveOccurred(),
			"first connection succeeded, want pending error")
		pending := store.Pending()
		Expect(pending).To(HaveLen(1), "Pending() = %v, want 1 key with fingerprint %s", pending, fingerprint)
		Expect(pending[0].Fingerprint).To(Equal(fingerprint), "Pending() = %v, want 1 key with fingerprint %s", pending, fingerprint)

		// Approve and reconnect.
		Expect(store.Approve("example.com", fingerprint)).NotTo(HaveOccurred(), "Approve")
		Expect(store.Callback()("example.com:22", testAddr("example.com"), key)).NotTo(HaveOccurred(), "approved key rejected")
		Expect(store.Pending()).To(BeEmpty(), "Pending() after approve = %v, want empty", store.Pending())

		// Persisted across restarts.
		reloaded, err := NewHostKeyStore(conf)
		Expect(err).NotTo(HaveOccurred(), "NewHostKeyStore (reload)")
		Expect(reloaded.Callback()("example.com:22", testAddr("example.com"), key)).NotTo(HaveOccurred(), "persisted key rejected")

		// Changed key is rejected.
		other, err := testKey()
		Expect(err).NotTo(HaveOccurred())
		err = reloaded.Callback()("example.com:22", testAddr("example.com"), other)
		Expect(err).To(HaveOccurred(), "changed key error = %v, want 'changed'", err)
		Expect(err.Error()).To(ContainSubstring("changed"), "changed key error = %v, want 'changed'", err)
	})
})

var _ = Describe("HostKeyStoreAuto", func() {
	It("auto-accepts the first key and keeps it stable", func() {
		dir := GinkgoT().TempDir()
		conf := cfg.SSHConfig{
			HostKeyPolicy:  "auto",
			AutoAcceptFile: filepath.Join(dir, "accepted.json"),
			PendingFile:    filepath.Join(dir, "pending.json"),
		}

		store, err := NewHostKeyStore(conf)
		Expect(err).NotTo(HaveOccurred(), "NewHostKeyStore")
		key, err := testKey()
		Expect(err).NotTo(HaveOccurred())

		Expect(store.Callback()("host:22", testAddr("host"), key)).NotTo(HaveOccurred(), "first key rejected")
		Expect(store.Callback()("host:22", testAddr("host"), key)).NotTo(HaveOccurred(), "same key rejected")
		Expect(store.Pending()).To(BeEmpty(), "auto policy created pending keys: %v", store.Pending())
	})
})

var _ = Describe("HostKeyStoreReject", func() {
	It("removes a pending key on rejection", func() {
		dir := GinkgoT().TempDir()
		conf := cfg.SSHConfig{
			HostKeyPolicy:  "api",
			AutoAcceptFile: filepath.Join(dir, "accepted.json"),
			PendingFile:    filepath.Join(dir, "pending.json"),
		}
		store, err := NewHostKeyStore(conf)
		Expect(err).NotTo(HaveOccurred(), "NewHostKeyStore")
		key, err := testKey()
		Expect(err).NotTo(HaveOccurred())
		_ = store.Callback()("host:22", testAddr("host"), key)

		Expect(store.Reject("host")).NotTo(HaveOccurred(), "Reject")
		Expect(store.Pending()).To(BeEmpty(), "Pending() after reject = %v, want empty", store.Pending())
	})
})

var _ = Describe("HostKeyStorePolicies", func() {
	for _, policy := range []cfg.HostKeyPolicy{"api", "auto", "insecure"} {
		It("accepts policy %q", func() {
			conf := cfg.SSHConfig{HostKeyPolicy: policy, AutoAcceptFile: filepath.Join(GinkgoT().TempDir(), "keys.json")}
			_, err := NewHostKeyStore(conf)
			Expect(err).NotTo(HaveOccurred(), "policy %q", policy)
		})
	}

	It("rejects the ca policy without any CA files", func() {
		conf := cfg.SSHConfig{HostKeyPolicy: "ca"}
		_, err := NewHostKeyStore(conf)
		Expect(err).To(HaveOccurred(), "ca policy without ca_files succeeded, want error")
	})

	It("rejects an unknown policy", func() {
		conf := cfg.SSHConfig{HostKeyPolicy: "bogus"}
		_, err := NewHostKeyStore(conf)
		Expect(err).To(HaveOccurred(), "unknown policy succeeded, want error")
	})
})

var _ = Describe("EncodeKey", func() {
	It("encodes a public key as standard base64", func() {
		key, err := testKey()
		Expect(err).NotTo(HaveOccurred())
		encoded := EncodeKey(key)
		_, err = base64.StdEncoding.DecodeString(encoded)
		Expect(err).NotTo(HaveOccurred(), "EncodeKey produced invalid base64")
	})
})

var _ = Describe("ManagerStats", func() {
	It("reports zero stats for an empty and a nil manager", func() {
		m := &Manager{clients: map[string]*ssh.Client{}}
		stats := m.Stats()
		Expect(stats.ActiveConnections).To(Equal(0), "Stats() = %+v, want zeros", stats)
		Expect(stats.TotalDials).To(Equal(0), "Stats() = %+v, want zeros", stats)

		var nilManager *Manager
		Expect(nilManager.Stats()).To(Equal(Stats{}), "nil manager Stats() = %+v, want zeros", nilManager.Stats())
	})
})

var _ = Describe("LoadSigner", func() {
	It("loads a PEM-encoded ed25519 signer from disk", func() {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		Expect(err).NotTo(HaveOccurred(), "generate key")
		pemBytes, err := ssh.MarshalPrivateKey(priv, "")
		Expect(err).NotTo(HaveOccurred(), "marshal key")
		path := filepath.Join(GinkgoT().TempDir(), "id_ed25519")
		Expect(os.WriteFile(path, pem.EncodeToMemory(pemBytes), 0o600)).NotTo(HaveOccurred(), "write key")

		signer, err := loadSigner(path, "")
		Expect(err).NotTo(HaveOccurred(), "loadSigner")
		Expect(signer).ToNot(BeNil(), "loadSigner returned nil signer")
	})
})
