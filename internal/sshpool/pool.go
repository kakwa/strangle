// Package sshpool owns long-lived SSH connections on behalf of the embedded
// Ansible interpreter. Connections are created lazily and reused across tasks.
package sshpool

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kakwa/strangle/internal/cfg"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Endpoint identifies a remote host.
type Endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
}

// ExecResult is the result of a remote command.
type ExecResult struct {
	ReturnCode int    `json:"return_code"`
	Stdout     []byte `json:"stdout"`
	Stderr     []byte `json:"stderr"`
}

// Manager is a pool of persistent SSH clients.
type Manager struct {
	conf     cfg.SSHConfig
	provider SignerProvider
	password string
	hostKeys *HostKeyStore

	mu         sync.Mutex
	dialMu     sync.Mutex
	clients    map[string]*ssh.Client
	retired    map[string][]*ssh.Client
	authExpiry map[string]time.Time
	inUse      map[string]int
	dials      int
}

// Stats describes the current state of the connection pool.
type Stats struct {
	ActiveConnections int `json:"active_connections"`
	TotalDials        int `json:"total_dials"`
}

// New builds a manager. A nil manager is returned when SSH is disabled.
func New(conf cfg.SSHConfig) (*Manager, error) {
	if !conf.Enabled {
		return nil, nil
	}

	m := &Manager{
		conf:       conf,
		password:   conf.Password,
		clients:    map[string]*ssh.Client{},
		retired:    map[string][]*ssh.Client{},
		authExpiry: map[string]time.Time{},
		inUse:      map[string]int{},
	}

	// Static credentials are only used when certificate issuance is not
	// configured. With a certificate backend the private key is the subject
	// key for the freshly issued certificate, and static <key>-cert.pub files
	// or agent signers would otherwise allow long-lived authentication.
	var static []ssh.Signer
	if conf.Certificate == nil {
		if conf.PrivateKeyFile != "" {
			signer, err := loadSigner(conf.PrivateKeyFile, conf.PrivateKeyPassphrase)
			if err != nil {
				return nil, err
			}
			static = append(static, signer)
		}

		if conf.UseAgent {
			sock := os.Getenv("SSH_AUTH_SOCK")
			if sock == "" {
				return nil, errors.New("ssh use_agent is enabled but SSH_AUTH_SOCK is unset")
			}
			conn, err := net.Dial("unix", sock)
			if err != nil {
				return nil, fmt.Errorf("connect to SSH agent: %w", err)
			}
			signers, err := agent.NewClient(conn).Signers()
			if err != nil {
				return nil, fmt.Errorf("list SSH agent keys: %w", err)
			}
			static = append(static, signers...)
		}

		if len(static) == 0 && conf.Password == "" {
			return nil, errors.New("ssh requires a private_key_file, use_agent, or password")
		}
	}

	provider, err := newSignerProvider(conf, static)
	if err != nil {
		return nil, err
	}
	m.provider = provider

	hostKeys, err := NewHostKeyStore(conf)
	if err != nil {
		_ = provider.Close()
		return nil, err
	}
	m.hostKeys = hostKeys

	return m, nil
}

// HostKeys returns the host key store for the API.
func (m *Manager) HostKeys() *HostKeyStore {
	if m == nil {
		return nil
	}
	return m.hostKeys
}

// Stats returns pool counters, useful for confirming connections are reused.
func (m *Manager) Stats() Stats {
	if m == nil {
		return Stats{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Stats{ActiveConnections: len(m.clients), TotalDials: m.dials}
}

func loadPrivateKey(path, passphrase string) (ssh.Signer, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, fmt.Errorf("read private key %q: %w", path, err)
	}

	if passphrase != "" {
		signer, err := ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("parse private key %q: %w", path, err)
		}
		return signer, nil
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse private key %q: %w", path, err)
	}
	return signer, nil
}

func loadSigner(path, passphrase string) (ssh.Signer, error) {
	signer, err := loadPrivateKey(path, passphrase)
	if err != nil {
		return nil, err
	}

	// If an OpenSSH user certificate accompanies the key, wrap the signer.
	certPath := expandHome(path) + "-cert.pub"
	if certData, certErr := os.ReadFile(certPath); certErr == nil {
		key, _, _, _, parseErr := ssh.ParseAuthorizedKey(certData)
		if parseErr != nil {
			return nil, fmt.Errorf("parse SSH certificate %q: %w", certPath, parseErr)
		}
		cert, ok := key.(*ssh.Certificate)
		if !ok {
			return nil, fmt.Errorf("%q is not an SSH certificate", certPath)
		}
		signer, err = ssh.NewCertSigner(cert, signer)
		if err != nil {
			return nil, fmt.Errorf("create certificate signer: %w", err)
		}
	}

	return signer, nil
}

// normalizeEndpoint applies the configured user and port defaults and returns
// the cache key used by both client() and drop().
func (m *Manager) normalizeEndpoint(ep Endpoint) (Endpoint, string, error) {
	if ep.Host == "" {
		return ep, "", errors.New("ssh: empty host")
	}
	if ep.User == "" {
		ep.User = m.conf.User
	}
	if ep.User == "" {
		return ep, "", errors.New("ssh: empty user")
	}
	if ep.Port == 0 {
		ep.Port = m.conf.Port
	}
	addr := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
	return ep, ep.User + "@" + addr, nil
}

// liveClient returns a pooled client that can be used for new work. An
// expired or explicitly dropped client is retired: closed immediately if
// idle, otherwise kept alive until its last in-flight task releases it.
func (m *Manager) liveClient(key string) (*ssh.Client, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	client, ok := m.clients[key]
	if !ok {
		return nil, false
	}
	if expires, expiring := m.authExpiry[key]; expiring && !time.Now().Before(expires) {
		m.retireLocked(key)
		return nil, false
	}
	return client, true
}

// retireLocked moves the active client aside so a fresh one can be dialed
// while older tasks finish. The caller must hold m.mu.
func (m *Manager) retireLocked(key string) {
	if client, ok := m.clients[key]; ok {
		m.retired[key] = append(m.retired[key], client)
		delete(m.clients, key)
	}
	delete(m.authExpiry, key)
	if m.inUse[key] == 0 {
		m.closeRetiredLocked(key)
	}
}

func (m *Manager) closeRetiredLocked(key string) {
	for _, client := range m.retired[key] {
		_ = client.Close()
	}
	delete(m.retired, key)
}

// acquire pins a pooled client for the duration of one operation so an
// expiring certificate cannot close a connection under an active task.
func (m *Manager) acquire(ep Endpoint) (*ssh.Client, string, error) {
	ep, key, err := m.normalizeEndpoint(ep)
	if err != nil {
		return nil, "", err
	}
	client, err := m.client(ep)
	if err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	current, ok := m.clients[key]
	retry := !ok || current != client
	if !retry {
		m.inUse[key]++
	}
	m.mu.Unlock()
	if retry {
		return m.acquire(ep)
	}
	return client, key, nil
}

func (m *Manager) release(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inUse[key]--
	if m.inUse[key] > 0 {
		return
	}
	delete(m.inUse, key)
	m.closeRetiredLocked(key)
}

func (m *Manager) client(ep Endpoint) (*ssh.Client, error) {
	ep, key, err := m.normalizeEndpoint(ep)
	if err != nil {
		return nil, err
	}
	if client, ok := m.liveClient(key); ok {
		return client, nil
	}

	// Serialize issuance and dialing so concurrent tasks do not each open a
	// connection or ask Vault to sign the same certificate.
	m.dialMu.Lock()
	defer m.dialMu.Unlock()
	if client, ok := m.liveClient(key); ok {
		return client, nil
	}

	signers, expires, err := m.provider.Signers(ep)
	if err != nil {
		return nil, err
	}
	auth := []ssh.AuthMethod{}
	if len(signers) > 0 {
		auth = append(auth, ssh.PublicKeys(signers...))
	}
	if m.password != "" {
		auth = append(auth, ssh.Password(m.password))
	}

	addr := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{
		User:            ep.User,
		Auth:            auth,
		HostKeyCallback: m.hostKeys.Callback(),
		Timeout:         timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", key, err)
	}

	m.mu.Lock()
	m.clients[key] = client
	if expires.IsZero() {
		delete(m.authExpiry, key)
	} else {
		m.authExpiry[key] = expires
	}
	m.dials++
	m.mu.Unlock()
	return client, nil
}

// Exec runs a command over a reused SSH connection.
func (m *Manager) Exec(ep Endpoint, command string, stdin []byte) (ExecResult, error) {
	client, key, err := m.acquire(ep)
	if err != nil {
		return ExecResult{}, err
	}
	defer m.release(key)

	session, err := client.NewSession()
	if err != nil {
		m.drop(ep)
		return ExecResult{}, fmt.Errorf("ssh session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	session.Stdin = bytes.NewReader(stdin)

	err = session.Run(command)
	result := ExecResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return result, nil
	}

	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		result.ReturnCode = exitErr.ExitStatus()
		return result, nil
	}
	m.drop(ep)
	return result, fmt.Errorf("ssh exec: %w", err)
}

// Put writes a local file to the remote host over SFTP.
func (m *Manager) Put(ep Endpoint, remotePath string, content []byte, mode uint32) error {
	client, key, err := m.acquire(ep)
	if err != nil {
		return err
	}
	defer m.release(key)

	sftp, err := newSFTPClient(client)
	if err != nil {
		return err
	}
	defer sftp.Close()

	file, err := sftp.Create(remotePath)
	if err != nil {
		return fmt.Errorf("sftp create %q: %w", remotePath, err)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return fmt.Errorf("sftp write %q: %w", remotePath, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("sftp close %q: %w", remotePath, err)
	}
	if mode != 0 {
		if err := sftp.Chmod(remotePath, os.FileMode(mode)); err != nil {
			return fmt.Errorf("sftp chmod %q: %w", remotePath, err)
		}
	}
	return nil
}

// Fetch reads a remote file over SFTP.
func (m *Manager) Fetch(ep Endpoint, remotePath string) ([]byte, error) {
	client, key, err := m.acquire(ep)
	if err != nil {
		return nil, err
	}
	defer m.release(key)

	sftp, err := newSFTPClient(client)
	if err != nil {
		return nil, err
	}
	defer sftp.Close()

	file, err := sftp.Open(remotePath)
	if err != nil {
		return nil, fmt.Errorf("sftp open %q: %w", remotePath, err)
	}
	defer file.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(file); err != nil {
		return nil, fmt.Errorf("sftp read %q: %w", remotePath, err)
	}
	return buf.Bytes(), nil
}

func (m *Manager) drop(ep Endpoint) {
	_, key, err := m.normalizeEndpoint(ep)
	if err != nil {
		return
	}
	m.dropKey(key)
}

func (m *Manager) dropKey(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retireLocked(key)
}

// Close closes all pooled connections and releases provider resources.
func (m *Manager) Close() {
	m.mu.Lock()
	for key, client := range m.clients {
		_ = client.Close()
		delete(m.clients, key)
		delete(m.authExpiry, key)
	}
	for key := range m.retired {
		m.closeRetiredLocked(key)
	}
	m.mu.Unlock()
	if m.provider != nil {
		_ = m.provider.Close()
	}
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
