package sshpool

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kakwa/strangle/internal/cfg"
	"golang.org/x/crypto/ssh"
)

// SignerProvider supplies SSH user signers for one dial. The returned time is
// the moment a connection authenticated with those signers must be dropped and
// re-established; the zero value means the credentials do not expire.
type SignerProvider interface {
	Signers(Endpoint) ([]ssh.Signer, time.Time, error)
	Close() error
}

// staticSignerProvider keeps the historical behavior of offering the same
// private key, agent keys and optional <key>-cert.pub on every dial.
type staticSignerProvider struct {
	signers []ssh.Signer
}

func (p *staticSignerProvider) Signers(Endpoint) ([]ssh.Signer, time.Time, error) {
	return p.signers, time.Time{}, nil
}

func (p *staticSignerProvider) Close() error { return nil }

func newSignerProvider(conf cfg.SSHConfig, static []ssh.Signer) (SignerProvider, error) {
	if conf.Certificate == nil {
		return &staticSignerProvider{signers: static}, nil
	}

	// Certificate providers sign the public half of this key. The private key
	// stays local; only the public key is sent to Vault.
	subject, err := loadPrivateKey(conf.PrivateKeyFile, conf.PrivateKeyPassphrase)
	if err != nil {
		return nil, err
	}

	switch conf.Certificate.Backend {
	case cfg.CertificateInternal:
		return newInternalCAProvider(conf, subject)
	case cfg.CertificateVault:
		return newVaultProvider(conf, subject)
	default:
		return nil, fmt.Errorf("unsupported certificate backend %q", conf.Certificate.Backend)
	}
}

// certPrincipals always includes the actual login user, even when the config
// only lists shared principal names.
func certPrincipals(configured []string, user string) []string {
	out := make([]string, 0, len(configured)+1)
	seen := map[string]bool{}
	for _, p := range append([]string{user}, configured...) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// renewalDeadline stops using a connection slightly before its certificate
// expires, so an in-flight task cannot fail mid-run on expiry.
func renewalDeadline(validBefore time.Time, ttl time.Duration) time.Time {
	now := time.Now()
	if validBefore.Before(now) || validBefore.IsZero() {
		validBefore = now.Add(ttl)
	}
	margin := ttl / 10
	if margin > 30*time.Second {
		margin = 30 * time.Second
	}
	if margin < time.Second {
		margin = time.Second
	}
	deadline := validBefore.Add(-margin)
	if !deadline.After(now) {
		deadline = validBefore
	}
	return deadline
}

// internalCAProvider signs short-lived user certificates with a local CA key.
type internalCAProvider struct {
	subject     ssh.Signer
	authority   ssh.Signer
	principals  []string
	keyIDPrefix string
	ttl         time.Duration

	mu        sync.Mutex
	signer    ssh.Signer
	refreshAt time.Time
}

func newInternalCAProvider(conf cfg.SSHConfig, subject ssh.Signer) (SignerProvider, error) {
	path := conf.Certificate.Internal.CAKeyFile
	if path == "" {
		return nil, fmt.Errorf("internal certificate backend requires ca_key_file")
	}
	data, err := os.ReadFile(expandHome(path))
	if err != nil {
		return nil, fmt.Errorf("read SSH certificate CA key %q: %w", path, err)
	}
	authority, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse SSH certificate CA key %q: %w", path, err)
	}
	return &internalCAProvider{
		subject:     subject,
		authority:   authority,
		principals:  append([]string(nil), conf.Certificate.Principals...),
		keyIDPrefix: conf.Certificate.KeyIDPrefix,
		ttl:         conf.Certificate.TTL,
	}, nil
}

func (p *internalCAProvider) Signers(ep Endpoint) ([]ssh.Signer, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now().UTC()
	if p.signer != nil && now.Before(p.refreshAt) {
		return []ssh.Signer{p.signer}, p.refreshAt, nil
	}

	validBefore := now.Add(p.ttl)
	cert := &ssh.Certificate{
		Key:             p.subject.PublicKey(),
		CertType:        ssh.UserCert,
		KeyId:           fmt.Sprintf("%s-%d", p.keyIDPrefix, now.Unix()),
		ValidPrincipals: certPrincipals(p.principals, ep.User),
		ValidAfter:      uint64(now.Add(-time.Minute).Unix()),
		ValidBefore:     uint64(validBefore.Unix()),
	}
	if err := cert.SignCert(rand.Reader, p.authority); err != nil {
		return nil, time.Time{}, fmt.Errorf("sign SSH user certificate: %w", err)
	}
	signer, err := ssh.NewCertSigner(cert, p.subject)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("create certificate signer: %w", err)
	}
	p.signer = signer
	p.refreshAt = renewalDeadline(validBefore, p.ttl)
	return []ssh.Signer{p.signer}, p.refreshAt, nil
}

func (p *internalCAProvider) Close() error { return nil }

// vaultProvider authenticates with AppRole and asks Vault's SSH secrets engine
// to sign the subject's public key. Only public keys and the requested
// principals/TTL cross the network.
type vaultProvider struct {
	conf        cfg.VaultCertificateConfig
	httpClient  *http.Client
	subject     ssh.Signer
	principals  []string
	keyIDPrefix string
	ttl         time.Duration

	mu         sync.Mutex
	token      string
	tokenUntil time.Time
	signer     ssh.Signer
	refreshAt  time.Time
}

type vaultLoginResponse struct {
	Auth struct {
		ClientToken   string `json:"client_token"`
		LeaseDuration int64  `json:"lease_duration"`
	} `json:"auth"`
}

type vaultSignResponse struct {
	Data struct {
		SignedKey string `json:"signed_key"`
	} `json:"data"`
}

func newVaultProvider(conf cfg.SSHConfig, subject ssh.Signer) (SignerProvider, error) {
	v := conf.Certificate.Vault
	if v.Mount == "" {
		v.Mount = "ssh"
	}
	if v.AuthMount == "" {
		v.AuthMount = "approle"
	}

	roleID, err := vaultSecret(v.RoleID, v.RoleIDFile, "role_id")
	if err != nil {
		return nil, err
	}
	secretID, err := vaultSecret(v.SecretID, v.SecretIDFile, "secret_id")
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if v.CACertFile != "" {
		pem, err := os.ReadFile(expandHome(v.CACertFile))
		if err != nil {
			return nil, fmt.Errorf("read Vault CA certificate %q: %w", v.CACertFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("Vault CA certificate %q contains no PEM certificates", v.CACertFile)
		}
		tlsConfig.RootCAs = pool
	}

	resolved := v
	resolved.RoleID = roleID
	resolved.SecretID = secretID
	return &vaultProvider{
		conf:        resolved,
		httpClient:  &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig}},
		subject:     subject,
		principals:  append([]string(nil), conf.Certificate.Principals...),
		keyIDPrefix: conf.Certificate.KeyIDPrefix,
		ttl:         conf.Certificate.TTL,
	}, nil
}

func vaultSecret(inline, path, label string) (string, error) {
	if path != "" {
		data, err := os.ReadFile(expandHome(path))
		if err != nil {
			return "", fmt.Errorf("read Vault AppRole %s file %q: %w", label, path, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if inline == "" {
		return "", fmt.Errorf("Vault AppRole %s is empty", label)
	}
	return inline, nil
}

func (p *vaultProvider) request(ctx context.Context, method, path string, body, out any, withToken bool) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method,
		strings.TrimRight(p.conf.Addr, "/")+"/v1/"+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build Vault request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.conf.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", p.conf.Namespace)
	}
	if withToken {
		req.Header.Set("X-Vault-Token", p.token)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Vault request %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read Vault response %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Vault request %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode Vault response %s: %w", path, err)
		}
	}
	return nil
}

func (p *vaultProvider) ensureToken(ctx context.Context) error {
	if p.token != "" && time.Now().Add(10*time.Second).Before(p.tokenUntil) {
		return nil
	}
	path := "auth/" + url.PathEscape(p.conf.AuthMount) + "/login"
	var login vaultLoginResponse
	err := p.request(ctx, http.MethodPost, path, map[string]string{
		"role_id":   p.conf.RoleID,
		"secret_id": p.conf.SecretID,
	}, &login, false)
	if err != nil {
		return err
	}
	if login.Auth.ClientToken == "" {
		return fmt.Errorf("Vault AppRole login returned no client token")
	}
	lease := time.Duration(login.Auth.LeaseDuration) * time.Second
	p.token = login.Auth.ClientToken
	if lease <= time.Minute {
		// Too short to use safely: the next issuance will log in again.
		p.tokenUntil = time.Now()
	} else {
		p.tokenUntil = time.Now().Add(lease - time.Minute)
	}
	return nil
}

func (p *vaultProvider) Signers(ep Endpoint) ([]ssh.Signer, time.Time, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	if p.signer != nil && now.Before(p.refreshAt) {
		return []ssh.Signer{p.signer}, p.refreshAt, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := p.ensureToken(ctx); err != nil {
		return nil, time.Time{}, err
	}

	path := url.PathEscape(p.conf.Mount) + "/sign/" + url.PathEscape(p.conf.Role)
	var signed vaultSignResponse
	err := p.request(ctx, http.MethodPost, path, map[string]string{
		"public_key":       strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p.subject.PublicKey()))),
		"valid_principals": strings.Join(certPrincipals(p.principals, ep.User), ","),
		"ttl":              p.ttl.String(),
		"key_id":           fmt.Sprintf("%s-%d", p.keyIDPrefix, now.Unix()),
	}, &signed, true)
	if err != nil {
		return nil, time.Time{}, err
	}

	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(signed.Data.SignedKey))
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("parse Vault SSH certificate: %w", err)
	}
	cert, ok := key.(*ssh.Certificate)
	if !ok {
		return nil, time.Time{}, fmt.Errorf("Vault returned %T, want an SSH certificate", key)
	}
	signer, err := ssh.NewCertSigner(cert, p.subject)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("create Vault certificate signer: %w", err)
	}
	p.signer = signer
	p.refreshAt = renewalDeadline(time.Unix(int64(cert.ValidBefore), 0), p.ttl)
	return []ssh.Signer{p.signer}, p.refreshAt, nil
}

func (p *vaultProvider) Close() error {
	if transport, ok := p.httpClient.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	return nil
}
