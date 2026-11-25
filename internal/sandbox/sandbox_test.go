package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kakwa/strangle/internal/cfg"
)

const (
	helperManifestEnv   = "STRANGLE_SANDBOX_TEST_MANIFEST"
	helperCollectionEnv = "STRANGLE_SANDBOX_TEST_COLLECTION"
	helperNeighborEnv   = "STRANGLE_SANDBOX_TEST_NEIGHBOR"
	helperScratchEnv    = "STRANGLE_SANDBOX_TEST_SCRATCH"
)

// TestSandboxHelperProcess is run as a child process by TestLandlockIsolation.
// Applying Landlock is irreversible, so the checks live in a subprocess.
func TestSandboxHelperProcess(t *testing.T) {
	manifestPath := os.Getenv(helperManifestEnv)
	if manifestPath == "" {
		return
	}
	m, err := ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(m); err != nil {
		t.Fatal(err)
	}

	collection := os.Getenv(helperCollectionEnv)
	neighbor := os.Getenv(helperNeighborEnv)
	scratch := os.Getenv(helperScratchEnv)

	if _, err := os.ReadFile(filepath.Join(collection, "own.txt")); err != nil {
		fmt.Println("cannot read own collection:", err)
		return
	}
	if _, err := os.ReadFile(neighbor); err == nil {
		fmt.Println("can read another collection's secret")
		return
	}
	if err := os.WriteFile(filepath.Join(collection, "new.txt"), []byte("x"), 0o600); err == nil {
		fmt.Println("can write to the collection")
		return
	}
	if err := os.WriteFile(filepath.Join(scratch, "new.txt"), []byte("x"), 0o600); err != nil {
		fmt.Println("cannot write to scratch:", err)
		return
	}
	fmt.Println("ok")
	// Avoid the test harness's PASS line; the parent compares exact output.
	os.Exit(0)
}

func TestLandlockIsolation(t *testing.T) {
	if !KernelSupported() {
		t.Skip("kernel does not support Landlock")
	}
	root := t.TempDir()
	collection := filepath.Join(root, "collection")
	neighborDir := filepath.Join(root, "neighbor")
	scratch := filepath.Join(root, "scratch")
	for _, dir := range []string{collection, neighborDir, scratch} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	neighbor := filepath.Join(neighborDir, "secret.txt")
	for path, data := range map[string]string{
		filepath.Join(collection, "own.txt"): "mine",
		neighbor:                             "theirs",
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	server := filepath.Join(root, "server.yml")
	collCfg := filepath.Join(root, "collection.yml")
	for _, p := range []string{server, collCfg} {
		if err := os.WriteFile(p, []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifests, err := Build(cfg.SandboxConfig{Enabled: true}, []cfg.TargetConfig{{
		Name: "test", Path: collection, WorkingDir: collection,
	}}, server, collCfg)
	if err != nil {
		t.Fatal(err)
	}
	m := manifests["test"]
	m.Rules = append(m.Rules, Rule{Path: scratch, Mode: ModeRW, Kind: KindDir, Refer: true, ResolveUnix: true})
	mPath := filepath.Join(root, "manifest.json")
	if err := m.WriteFile(mPath); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestSandboxHelperProcess$")
	cmd.Env = append(os.Environ(),
		helperManifestEnv+"="+mPath,
		helperCollectionEnv+"="+collection,
		helperNeighborEnv+"="+neighbor,
		helperScratchEnv+"="+scratch,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox helper failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "ok" {
		t.Fatalf("sandbox helper output = %q, want ok", got)
	}
}

func TestBuildAllowsCertificateSecretsReadOnly(t *testing.T) {
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"key":      filepath.Join(secrets, "id_ed25519"),
		"ca":       filepath.Join(secrets, "ssh_ca_key"),
		"role":     filepath.Join(secrets, "role_id"),
		"secret":   filepath.Join(secrets, "secret_id"),
		"vault-ca": filepath.Join(secrets, "vault-ca.pem"),
	}
	for _, p := range paths {
		if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := cfg.TargetConfig{
		Name: "home", Path: filepath.Join(root, "collection"), WorkingDir: filepath.Join(root, "collection"),
		Playbooks: []cfg.PlaybookConfig{{
			Name: "site",
			Options: cfg.PlaybookOptions{SSH: cfg.SSHConfig{
				Enabled:        true,
				PrivateKeyFile: paths["key"],
				Certificate: &cfg.CertificateConfig{
					Backend:  cfg.CertificateVault,
					Internal: cfg.InternalCAConfig{CAKeyFile: paths["ca"]},
					Vault: cfg.VaultCertificateConfig{
						CACertFile:   paths["vault-ca"],
						RoleIDFile:   paths["role"],
						SecretIDFile: paths["secret"],
					},
				},
			}},
		}},
	}
	manifests, err := Build(cfg.SandboxConfig{Enabled: true}, []cfg.TargetConfig{target},
		filepath.Join(root, "server.yml"), filepath.Join(root, "collection.yml"))
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, r := range manifests["home"].Rules {
		modes[r.Path] = r.Mode
	}
	for _, p := range paths {
		if modes[p] != ModeRO {
			t.Errorf("rule for %q = %q, want %q", p, modes[p], ModeRO)
		}
	}
}

func TestBuildKeepsCollectionsReadOnly(t *testing.T) {
	root := t.TempDir()
	collDir := filepath.Join(root, "collection")
	if err := os.MkdirAll(collDir, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(collDir, "site.auto.json")
	target := cfg.TargetConfig{
		Name: "home", Path: collDir, WorkingDir: collDir,
		Playbooks: []cfg.PlaybookConfig{{
			Name: "site",
			Options: cfg.PlaybookOptions{SSH: cfg.SSHConfig{
				Enabled:        true,
				AutoAcceptFile: keyFile,
			}},
		}},
	}
	manifests, err := Build(cfg.SandboxConfig{
		Enabled:  true,
		Writable: []string{collDir},
	}, []cfg.TargetConfig{target}, filepath.Join(root, "server.yml"), filepath.Join(root, "collection.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m := manifests["home"]

	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("host-key file was not created: %v", err)
	}
	for _, r := range m.Rules {
		if r.Path == collDir {
			if r.Mode != ModeRO {
				t.Errorf("collection rule mode = %q, want %q", r.Mode, ModeRO)
			}
			continue
		}
		if r.Mode == ModeRW && strings.HasPrefix(r.Path, collDir+string(filepath.Separator)) && r.Path != keyFile {
			t.Errorf("unexpected writable path inside collection: %q", r.Path)
		}
	}
	var keyRule bool
	for _, r := range m.Rules {
		if r.Path == keyFile && r.Mode == ModeRW {
			keyRule = true
		}
	}
	if !keyRule {
		t.Error("worker host-key file is not writable")
	}
}
