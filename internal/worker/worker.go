// Package worker runs strangle playbook jobs: the worker process serves the
// Runner gRPC service over a unix socket (worker.go), and the master side
// spawns, dials and dispatches to worker child processes (manager.go).
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"

	runnerv1 "github.com/kakwa/strangle/proto/strangle/v1"
	"go.uber.org/zap"

	"github.com/kakwa/strangle/internal/ansible"
	"github.com/kakwa/strangle/internal/sshpool"
)

// Server implements the Runner gRPC service inside one worker process: it
// owns the embedded Ansible interpreter and the SSH managers and executes
// the playbooks the master dispatches to it.
type Server struct {
	runnerv1.UnimplementedRunnerServer

	// mu serializes runs: each worker owns exactly one embedded
	// interpreter, and the SSH scope (SetActive) must stay fixed for the
	// whole run, so scope selection and execution are one critical section.
	mu     sync.Mutex
	runner *ansible.Runner
	multi  *sshpool.Multi
}

// NewServer returns the gRPC service backed by the given runner. multi may be
// nil when no playbook uses the Go-managed SSH transport.
func NewServer(runner *ansible.Runner, multi *sshpool.Multi) *Server {
	return &Server{runner: runner, multi: multi}
}

// Ping reports that the worker is ready to run playbooks.
func (s *Server) Ping(_ context.Context, _ *runnerv1.PingRequest) (*runnerv1.PingResponse, error) {
	return &runnerv1.PingResponse{Ok: true}, nil
}

// RunPlaybook executes one playbook and returns its exit code and captured
// output. Consecutive calls are serialized by s.mu.
func (s *Server) RunPlaybook(ctx context.Context, req *runnerv1.RunPlaybookRequest) (*runnerv1.RunPlaybookResponse, error) {
	s.mu.Lock()

	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return &runnerv1.RunPlaybookResponse{Error: err.Error()}, nil
	}

	// The bridge carries no scope information, so point the SSH dispatcher
	// at this job's manager for the duration of the run.
	if s.multi != nil {
		if req.Scope != "" {
			if err := s.multi.SetActive(req.Scope); err != nil {
				s.mu.Unlock()
				return &runnerv1.RunPlaybookResponse{Error: err.Error()}, nil
			}
		} else {
			s.multi.ClearActive()
		}
	}

	// RunDir blocks on the embedded interpreter and cannot be interrupted.
	// Run it in a goroutine so the gRPC call can return promptly when the
	// master cancels the job. The mutex is released only when the interpreter
	// finishes, so a new run never overlaps the cancelled one.
	type runReply struct {
		result ansible.Result
		err    error
	}
	reply := make(chan runReply, 1)
	go func() {
		result, err := s.runner.RunDir(req.WorkingDir, req.Args...)
		reply <- runReply{result: result, err: err}
	}()

	select {
	case res := <-reply:
		s.mu.Unlock()
		factsJSON, _ := json.Marshal(res.result.Facts)
		return &runnerv1.RunPlaybookResponse{
			ExitCode:  int32(res.result.ExitCode),
			Output:    res.result.Output,
			Error:     errorString(res.err),
			FactsJson: string(factsJSON),
		}, nil
	case <-ctx.Done():
		go func() {
			<-reply
			s.mu.Unlock()
		}()
		return &runnerv1.RunPlaybookResponse{
			Error: "playbook run canceled",
		}, nil
	}
}

// Run serves srv on the unix socket at path until ctx is canceled, then
// stops the gRPC server. In-flight runs are given up to stopTimeout to
// finish before the socket is closed.
func Run(ctx context.Context, path string, srv *grpc.Server, logger *zap.Logger, stopTimeout time.Duration) error {
	lis, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", path, err)
	}
	defer func() { _ = lis.Close() }()

	logger.Info("worker serving", zap.String("socket", path))

	go func() {
		<-ctx.Done()
		done := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(stopTimeout):
			srv.Stop()
		}
	}()

	if err := srv.Serve(lis); err != nil {
		return fmt.Errorf("serve on %q: %w", path, err)
	}
	return nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
