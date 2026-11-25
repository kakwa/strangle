package config

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const strangleInternalCert = `
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
    options:
      ssh:
        enabled: true
        private_key_file: id_ed25519
        host_key_policy: auto
        certificate:
          backend: internal
          principals: [deploy, ubuntu]
          internal:
            ca_key_file: /etc/strangle/ssh_ca
`

func TestLoadInternalCertificateConfig(t *testing.T) {
	serverPath, collectionPath := writeTree(t, "listen_addr: \":9000\"\n", collOne, strangleInternalCert)
	cfg, err := Load(serverPath, collectionPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	target, ok := cfg.Target("one")
	if !ok || len(target.Playbooks) != 1 {
		t.Fatalf("Target(one) = %+v", target)
	}
	cert := target.Playbooks[0].Options.SSH.Certificate
	if cert == nil {
		t.Fatal("Certificate = nil, want an internal CA provider")
	}
	if cert.Backend != CertificateInternal || cert.TTL != 10*time.Minute ||
		cert.KeyIDPrefix != "strangle" || cert.Internal.CAKeyFile != "/etc/strangle/ssh_ca" {
		t.Errorf("Certificate = %+v, want internal defaults", cert)
	}
	if !slices.Equal(cert.Principals, []string{"deploy", "ubuntu"}) {
		t.Errorf("Principals = %v", cert.Principals)
	}
}

func TestLoadCertificateMergeAndVaultDefaults(t *testing.T) {
	strangle := `
options:
  ssh:
    enabled: true
    private_key_file: id_ed25519
    host_key_policy: auto
    certificate:
      backend: vault
      ttl: 15m
      key_id_prefix: lab
      vault:
        addr: https://vault.example.com
        role: strangle
        role_id_file: /run/secrets/role_id
        secret_id_file: /run/secrets/secret_id
        ca_cert_file: /etc/ssl/vault-ca.pem
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory
    options:
      ssh:
        certificate:
          ttl: 30m
          principals: [deploy]
          vault:
            mount: ssh-lab
`
	serverPath, collectionPath := writeTree(t, "listen_addr: \":9000\"\n", collOne, strangle)
	cfg, err := Load(serverPath, collectionPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	target, _ := cfg.Target("one")
	cert := target.Playbooks[0].Options.SSH.Certificate
	if cert == nil {
		t.Fatal("Certificate = nil")
	}
	if cert.TTL != 30*time.Minute || cert.KeyIDPrefix != "lab" {
		t.Errorf("merged certificate = %+v, want playbook TTL and target prefix", cert)
	}
	if !slices.Equal(cert.Principals, []string{"deploy"}) {
		t.Errorf("Principals = %v", cert.Principals)
	}
	if cert.Vault.Mount != "ssh-lab" || cert.Vault.AuthMount != "approle" {
		t.Errorf("Vault mounts = %+v, want ssh-lab/approle", cert.Vault)
	}
	if cert.Vault.RoleIDFile != "/run/secrets/role_id" || cert.Vault.SecretIDFile != "/run/secrets/secret_id" {
		t.Errorf("Vault AppRole files = %+v", cert.Vault)
	}
}

func TestLoadCertificateValidation(t *testing.T) {
	cases := []struct {
		name     string
		ssh      string
		contains string
	}{
		{
			name: "unknown backend",
			ssh: `private_key_file: id
        certificate:
          backend: other`,
			contains: "backend must be",
		},
		{
			name: "missing private key",
			ssh: `certificate:
          backend: internal
          internal:
            ca_key_file: /etc/ca`,
			contains: "requires ssh.private_key_file",
		},
		{
			name: "agent not supported",
			ssh: `private_key_file: id
        use_agent: true
        certificate:
          backend: internal
          internal:
            ca_key_file: /etc/ca`,
			contains: "does not support ssh.use_agent",
		},
		{
			name: "invalid ttl",
			ssh: `private_key_file: id
        certificate:
          backend: internal
          ttl: 10s
          internal:
            ca_key_file: /etc/ca`,
			contains: "between 1m and 24h",
		},
		{
			name: "internal without ca key",
			ssh: `private_key_file: id
        certificate:
          backend: internal`,
			contains: "requires internal.ca_key_file",
		},
		{
			name: "vault without role",
			ssh: `private_key_file: id
        certificate:
          backend: vault
          vault:
            addr: https://vault.example.com
            role_id: id
            secret_id: secret`,
			contains: "vault.role is required",
		},
		{
			name: "vault without app role credentials",
			ssh: `private_key_file: id
        certificate:
          backend: vault
          vault:
            addr: https://vault.example.com
            role: strangle`,
			contains: "requires role_id/secret_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			strangle := "playbooks:\n  - name: site\n    playbook: site.yml\n    inventory: inventory\n    options:\n      ssh:\n        enabled: true\n        " +
				tc.ssh + "\n"
			serverPath, collectionPath := writeTree(t, "listen_addr: \":9000\"\n", collOne, strangle)
			_, err := Load(serverPath, collectionPath)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("Load() error = %v, want substring %q", err, tc.contains)
			}
		})
	}
}
