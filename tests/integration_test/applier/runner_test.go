package applier_test

import (
	"github.com/kakwa/strangle/controller/applier"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunner(t *testing.T) {
	runner, err := applier.NewRunner(nil)
	if err != nil {
		t.Fatalf("applier.NewRunner() error: %v", err)
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

	// Local connection avoids SSH for the embedded-interpreter tests below.
	local := []string{"-i", "localhost,", "-e", "ansible_connection=local"}

	t.Run("facts", func(t *testing.T) {
		playbook := writePlaybook(t, dir, "facts.yml", `
- hosts: all
  gather_facts: true
  tasks:
    - name: Use facts
      ansible.builtin.debug:
        msg: "hello {{ ansible_hostname }}"
`)
		args := append([]string{"--diff"}, append(local, playbook)...)
		result, err := runner.Run(args...)
		skipIfAnsibleMissing(t, err)
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("ExitCode = %d, want 0; output:\n%s", result.ExitCode, result.Output)
		}
		facts, ok := result.Facts["localhost"]
		if !ok || !strings.Contains(string(facts), "ansible_hostname") {
			t.Fatalf("Facts = %v, want ansible_facts for localhost; output:\n%s", result.Facts, result.Output)
		}
	})

	t.Run("facts are not carried between runs", func(t *testing.T) {
		playbook := writePlaybook(t, dir, "no-facts.yml", `
- hosts: all
  gather_facts: false
  tasks:
    - name: Say hello
      ansible.builtin.debug:
        msg: hello
`)
		args := append([]string{"--diff"}, append(local, playbook)...)
		result, err := runner.Run(args...)
		skipIfAnsibleMissing(t, err)
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		if len(result.Facts) != 0 {
			t.Fatalf("Facts = %v, want no facts; output:\n%s", result.Facts, result.Output)
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

	// These subtests verify that output is not degraded when playbooks run
	// repeatedly in the same process: --diff output must appear every time
	// (the CLI args are rebuilt per run) and repeated error messages must
	// not be swallowed by Ansible's process-wide display deduplication.
	t.Run("diff", func(t *testing.T) {
		dest := filepath.Join(dir, "diff.txt")
		playbook := writePlaybook(t, dir, "diff.yml", `
- hosts: all
  gather_facts: false
  tasks:
    - name: Write marker
      ansible.builtin.copy:
        content: "marker\n"
        dest: `+dest+`
`)
		_ = os.Remove(dest)
		result, err := runner.Run(append([]string{"--diff"}, append(local, playbook)...)...)
		skipIfAnsibleMissing(t, err)
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		if !strings.Contains(result.Output, "--- before") || !strings.Contains(result.Output, "+++ after:") {
			t.Errorf("Output does not contain the diff block:\n%s", result.Output)
		}
	})

	t.Run("repeated error", func(t *testing.T) {
		playbook := writePlaybook(t, dir, "repeat-fail.yml", `
- hosts: all
  gather_facts: false
  tasks:
    - name: Fail on purpose
      ansible.builtin.fail:
        msg: repeat-me failure
`)
		args := append([]string{"--diff"}, append(local, playbook)...)
		for i := 0; i < 2; i++ {
			result, err := runner.Run(args...)
			skipIfAnsibleMissing(t, err)
			if err != nil {
				t.Fatalf("Run() error: %v", err)
			}
			if !strings.Contains(result.Output, "[ERROR]:") {
				t.Fatalf("Run %d output does not contain the error block:\n%s", i+1, result.Output)
			}
			if !strings.Contains(result.Output, "repeat-me failure") {
				t.Fatalf("Run %d output does not contain the failure message:\n%s", i+1, result.Output)
			}
		}
	})

	// Regression: Ansible's TaskQueueManager replaces the process-wide
	// SIGINT/SIGTERM handlers with its own during a run, which used to
	// clobber the C-level handler os/signal installs. After a playbook run
	// the signal must still be delivered to a Notify channel.
	t.Run("signal delivery", func(t *testing.T) {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM)
		defer signal.Stop(ch)

		result, err := runner.Run(append([]string{"--diff"}, append(local, okPlaybook)...)...)
		skipIfAnsibleMissing(t, err)
		if err != nil {
			t.Fatalf("Run() error: %v", err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("ExitCode = %d, want 0; output:\n%s", result.ExitCode, result.Output)
		}

		syscall.Kill(os.Getpid(), syscall.SIGTERM)
		select {
		case s := <-ch:
			if s != syscall.SIGTERM {
				t.Fatalf("received unexpected signal %v", s)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("SIGTERM was not delivered to the os/signal channel after a playbook run")
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
