package workeripc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"os"
	"time"
)

// Client is one JSON-RPC client talking to a worker process over its unix
// socket. rpc.Client supports concurrent calls and serializes their writes.
type Client struct {
	client *rpc.Client
	path   string
}

// Dial connects to the worker listening on the unix socket at path, waiting
// until the socket exists and accepts connections. It deliberately does not
// call Ping here: an unresponsive worker must still produce a client so the
// supervisor can probe and kill it.
func Dial(ctx context.Context, path string) (*Client, error) {
	if err := waitForSocket(ctx, path); err != nil {
		return nil, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dialSocket(waitCtx, path)
	if err != nil {
		return nil, fmt.Errorf("wait for worker %q: %w", path, err)
	}

	return &Client{
		client: rpc.NewClientWithCodec(jsonrpc.NewClientCodec(conn)),
		path:   path,
	}, nil
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

// dialSocket polls until the worker's socket accepts a connection. The
// socket may be bound slightly before the worker starts serving.
func dialSocket(ctx context.Context, path string) (net.Conn, error) {
	for {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			return conn, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// call runs one RPC and stops waiting when ctx is done. net/rpc has no
// context-aware Call, so Go plus a done channel provides the cancellation;
// a late reply is simply ignored and the connection stays usable.
func (c *Client) call(ctx context.Context, method string, args, reply any) error {
	done := make(chan *rpc.Call, 1)
	call := c.client.Go(method, args, reply, done)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return call.Error
	}
}

// Ping checks that the worker is alive and ready.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var reply PingReply
	if err := c.call(ctx, pingMethod, PingArgs{}, &reply); err != nil {
		return err
	}
	if !reply.OK {
		return errors.New("worker ping: not ok")
	}
	return nil
}

// RunPlaybook dispatches one playbook to the worker and returns its exit
// code, captured output and per-host facts. Cancelling ctx stops waiting for
// the reply; the worker keeps running the interpreter until it finishes, so
// its next run cannot overlap the cancelled one.
func (c *Client) RunPlaybook(ctx context.Context, collection, scope, workingDir string, args ...string) (int, string, map[string]json.RawMessage, error) {
	var reply RunReply
	err := c.call(ctx, runMethod, RunArgs{
		Collection: collection,
		Scope:      scope,
		WorkingDir: workingDir,
		Args:       args,
	}, &reply)
	if err != nil {
		return 0, "", nil, fmt.Errorf("run playbook on worker: %w", err)
	}
	if reply.Error != "" {
		return reply.ExitCode, reply.Output, reply.Facts, errors.New(reply.Error)
	}
	return reply.ExitCode, reply.Output, reply.Facts, nil
}

// Close closes the connection to the worker.
func (c *Client) Close() error {
	return c.client.Close()
}

// Ready reports whether the worker's socket currently accepts connections.
func (c *Client) Ready() bool {
	conn, err := net.DialTimeout("unix", c.path, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
