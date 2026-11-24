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

	"github.com/kakwa/strangle/internal/config"
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
	cfg      config.SSHConfig
	signers  []ssh.Signer
	password string
	hostKeys *HostKeyStore

	mu      sync.Mutex
	clients map[string]*ssh.Client
	dials   int
}

// Stats describes the current state of the connection pool.
type Stats struct {
	ActiveConnections int `json:"active_connections"`
	TotalDials        int `json:"total_dials"`
}

// New builds a manager. A nil manager is returned when SSH is disabled.
func New(cfg config.SSHConfig) (*Manager, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	m := &Manager{
		cfg:      cfg,
		password: cfg.Password,
		clients:  map[string]*ssh.Client{},
	}

	if cfg.PrivateKeyFile != "" {
		signer, err := loadSigner(cfg.PrivateKeyFile, cfg.PrivateKeyPassphrase)
		if err != nil {
			return nil, err
		}
		m.signers = append(m.signers, signer)
	}

	if cfg.UseAgent {
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
		m.signers = append(m.signers, signers...)
	}

	if len(m.signers) == 0 && cfg.Password == "" {
		return nil, errors.New("ssh requires a private_key_file, use_agent, or password")
	}

	hostKeys, err := NewHostKeyStore(cfg)
	if err != nil {
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

func loadSigner(path, passphrase string) (ssh.Signer, error) {
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, fmt.Errorf("read private key %q: %w", path, err)
	}

	var signer ssh.Signer
	if passphrase != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(data, []byte(passphrase))
	} else {
		signer, err = ssh.ParsePrivateKey(data)
	}
	if err != nil {
		return nil, fmt.Errorf("parse private key %q: %w", path, err)
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

func (m *Manager) client(ep Endpoint) (*ssh.Client, error) {
	if ep.Host == "" {
		return nil, errors.New("ssh: empty host")
	}
	if ep.User == "" {
		ep.User = m.cfg.User
	}
	if ep.User == "" {
		return nil, errors.New("ssh: empty user")
	}
	if ep.Port == 0 {
		ep.Port = m.cfg.Port
	}

	addr := net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
	key := ep.User + "@" + addr

	m.mu.Lock()
	if client, ok := m.clients[key]; ok {
		m.mu.Unlock()
		return client, nil
	}
	m.mu.Unlock()

	auth := []ssh.AuthMethod{}
	if len(m.signers) > 0 {
		auth = append(auth, ssh.PublicKeys(m.signers...))
	}
	if m.password != "" {
		auth = append(auth, ssh.Password(m.password))
	}

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
	m.dials++
	m.mu.Unlock()
	return client, nil
}

// Exec runs a command over a reused SSH connection.
func (m *Manager) Exec(ep Endpoint, command string, stdin []byte) (ExecResult, error) {
	client, err := m.client(ep)
	if err != nil {
		return ExecResult{}, err
	}

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
	client, err := m.client(ep)
	if err != nil {
		return err
	}

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
	client, err := m.client(ep)
	if err != nil {
		return nil, err
	}

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
	key := ep.User + "@" + net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port))
	m.mu.Lock()
	defer m.mu.Unlock()
	if client, ok := m.clients[key]; ok {
		client.Close()
		delete(m.clients, key)
	}
}

// Close closes all pooled connections.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, client := range m.clients {
		_ = client.Close()
		delete(m.clients, key)
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
