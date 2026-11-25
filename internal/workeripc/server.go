package workeripc

import (
	"context"
	"fmt"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Handler is implemented by the worker process, which owns the interpreter
// that actually runs playbooks. Ping is answered by the protocol itself.
type Handler interface {
	RunPlaybook(RunArgs) RunReply
}

type service struct {
	h Handler
}

func (s service) Ping(_ PingArgs, reply *PingReply) error {
	reply.OK = true
	return nil
}

func (s service) RunPlaybook(args RunArgs, reply *RunReply) error {
	*reply = s.h.RunPlaybook(args)
	return nil
}

// NewRPCServer registers h as the Worker service. Registration can only fail
// if one of the fixed method signatures above is wrong.
func NewRPCServer(h Handler) *rpc.Server {
	srv := rpc.NewServer()
	if err := srv.RegisterName(serviceName, service{h}); err != nil {
		panic(err)
	}
	return srv
}

// Serve serves srv on the unix socket at path until ctx is canceled. Each
// connection gets its own JSON-RPC codec; in-flight calls are given up to
// stopTimeout to finish before their connections are closed.
func Serve(ctx context.Context, path string, srv *rpc.Server, logger *zap.Logger, stopTimeout time.Duration) error {
	lis, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", path, err)
	}
	defer func() { _ = lis.Close() }()

	var (
		mu      sync.Mutex
		conns   = map[net.Conn]struct{}{}
		wg      sync.WaitGroup
		stopped = make(chan struct{})
	)
	serveDone := make(chan struct{})
	defer close(serveDone)
	go func() {
		select {
		case <-ctx.Done():
			close(stopped)
			_ = lis.Close() // unblocks Accept
		case <-serveDone:
		}
	}()

	logger.Info("worker serving", zap.String("socket", path))

	for {
		conn, err := lis.Accept()
		if err != nil {
			select {
			case <-stopped:
				waitConns(&wg, &mu, conns, stopTimeout)
				return nil
			default:
				return fmt.Errorf("accept on %q: %w", path, err)
			}
		}

		mu.Lock()
		conns[conn] = struct{}{}
		mu.Unlock()
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			srv.ServeCodec(jsonrpc.NewServerCodec(c))
			mu.Lock()
			delete(conns, c)
			mu.Unlock()
			_ = c.Close()
		}(conn)
	}
}

// waitConns lets in-flight calls finish, then closes any connections still
// busy after the timeout.
func waitConns(wg *sync.WaitGroup, mu *sync.Mutex, conns map[net.Conn]struct{}, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(timeout):
		mu.Lock()
		for conn := range conns {
			_ = conn.Close()
		}
		mu.Unlock()
		<-done
	}
}
