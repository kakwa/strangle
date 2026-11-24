package worker

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	runnerv1 "github.com/kakwa/strangle/proto/strangle/v1"
)

// MaxRecvBytes is the gRPC message size limit for a single run's captured
// output. Ansible runs against large inventories can produce several
// megabytes of output.
const MaxRecvBytes = 64 << 20

// Client is one connection to a worker process.
type Client struct {
	conn *grpc.ClientConn
	cli  runnerv1.RunnerClient
}

// Dial connects to the worker listening on the unix socket at path, waiting
// until the socket exists and the connection is ready.
func Dial(ctx context.Context, path string) (*Client, error) {
	if err := waitForSocket(ctx, path); err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(
		"passthrough:///"+path,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxRecvBytes)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial worker %q: %w", path, err)
	}

	// NewClient is lazy: nothing dials until Connect is called or an RPC
	// is made, so kick off the connection explicitly.
	conn.Connect()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for state := conn.GetState(); state != connectivity.Ready; {
		if !conn.WaitForStateChange(waitCtx, state) {
			_ = conn.Close()
			return nil, fmt.Errorf("wait for worker %q: %w", path, waitCtx.Err())
		}
		state = conn.GetState()
	}

	return &Client{conn: conn, cli: runnerv1.NewRunnerClient(conn)}, nil
}

// waitForSocket polls until the socket file appears or ctx is done.
func waitForSocket(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat socket %q: %w", path, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for socket %q: %w", path, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Ping checks that the worker is alive and ready.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.cli.Ping(ctx, &runnerv1.PingRequest{})
	return err
}

// RunPlaybook dispatches one playbook to the worker and returns its exit
// code and captured output.
func (c *Client) RunPlaybook(ctx context.Context, scope, workingDir string, args ...string) (int32, string, error) {
	res, err := c.cli.RunPlaybook(ctx, &runnerv1.RunPlaybookRequest{
		Scope:      scope,
		WorkingDir: workingDir,
		Args:       args,
	})
	if err != nil {
		return 0, "", fmt.Errorf("run playbook on worker: %w", err)
	}
	if res.Error != "" {
		return res.ExitCode, res.Output, fmt.Errorf("%s", res.Error)
	}
	return res.ExitCode, res.Output, nil
}

// Close closes the connection to the worker.
func (c *Client) Close() error {
	return c.conn.Close()
}

// Ready reports whether the connection is in the READY state.
func (c *Client) Ready() bool {
	return c.conn.GetState() == connectivity.Ready
}
