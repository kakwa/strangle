// Package worker runs strangle playbook jobs: the worker process executes
// playbooks in its embedded interpreter (worker.go), while the master side
// spawns, supervises and dispatches to worker child processes (manager.go).
// The two halves talk over the JSON-RPC service in rpc_*.go.
package worker

import (
	"sync"

	"github.com/kakwa/strangle/internal/ansible"
	"github.com/kakwa/strangle/internal/sshpool"
)

// Server implements the worker side of the JSON-RPC protocol. It owns the
// embedded Ansible interpreter and the SSH managers.
type Server struct {
	// mu serializes runs: each worker owns exactly one embedded
	// interpreter, and the SSH scope (SetActive) must stay fixed for the
	// whole run, so scope selection and execution are one critical section.
	mu     sync.Mutex
	runner *ansible.Runner
	multi  *sshpool.Multi
}

// NewServer returns the worker service backed by the given runner. multi may
// be nil when no playbook uses the Go-managed SSH transport.
func NewServer(runner *ansible.Runner, multi *sshpool.Multi) *Server {
	return &Server{runner: runner, multi: multi}
}

// RunPlaybook executes one playbook and returns its exit code, captured
// output and per-host facts. net/rpc runs each call in its own goroutine;
// the mutex keeps runs on this worker from overlapping.
func (s *Server) RunPlaybook(req RunArgs) RunReply {
	s.mu.Lock()
	defer s.mu.Unlock()

	// The bridge carries no scope information, so point the SSH dispatcher
	// at this job's manager for the duration of the run.
	if s.multi != nil {
		if req.Scope != "" {
			if err := s.multi.SetActive(req.Scope); err != nil {
				return RunReply{Error: err.Error()}
			}
		} else {
			s.multi.ClearActive()
		}
	}

	result, err := s.runner.RunDir(req.WorkingDir, req.Args...)
	return RunReply{
		ExitCode: result.ExitCode,
		Output:   result.Output,
		Error:    errorString(err),
		Facts:    result.Facts,
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
