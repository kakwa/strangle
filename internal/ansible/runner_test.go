package ansible

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunner(t *testing.T) {
	runner, err := NewRunner(nil)
	if err != nil {
		t.Fatalf("NewRunner() error: %v", err)
	}
	defer runner.Close()

	dir := t.TempDir()
	okPlaybook := writePlaybook(t, dir, "ok.yml", `
- hosts: all
  gather_facts: false
  tasks:
    - name: Say hello
      ansible.builtin.debug:
        msg: hello
`)
	failPlaybook := writePlaybook(t, dir, "fail.yml", `
- hosts: all
  gather_facts: false
  tasks:
    - name: Fail on purpose
      ansible.builtin.fail:
        msg: expected failure
`)

	t.Run("success", func(t *testing.T) {
		result, err := runner.Run("--diff", "-i", "localhost,", okPlaybook)
		skipIfAnsibleMissing(t, err)
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		if result.ExitCode != 0 {
			t.Errorf("ExitCode = %d, want 0; output:\n%s", result.ExitCode, result.Output)
		}
		if !strings.Contains(result.Output, "PLAY RECAP") {
			t.Errorf("Output does not contain the play recap:\n%s", result.Output)
		}
	})

	t.Run("failure", func(t *testing.T) {
		result, err := runner.Run("--diff", "-i", "localhost,", failPlaybook)
		skipIfAnsibleMissing(t, err)
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		if result.ExitCode != 2 {
			t.Errorf("ExitCode = %d, want 2; output:\n%s", result.ExitCode, result.Output)
		}
		if !strings.Contains(result.Output, "expected failure") {
			t.Errorf("Output does not contain the failure message:\n%s", result.Output)
		}
	})
}

func writePlaybook(t *testing.T, dir, name, data string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	return path
}

func skipIfAnsibleMissing(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), "No module named 'ansible'") {
		t.Skip("ansible is not installed in the embedded Python environment")
	}
}
