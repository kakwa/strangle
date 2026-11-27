package worker

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type testHandler struct {
	run func(RunArgs) RunReply
}

func (h testHandler) RunPlaybook(args RunArgs) RunReply {
	return h.run(args)
}

// startServer serves a protocol handler on a private unix socket and returns
// a client connected to it.
func startServer(t *testing.T, h Handler) *Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Serve(ctx, path, NewRPCServer(h), zap.NewNop(), time.Second); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	cli, err := Dial(ctx, path)
	if err != nil {
		cancel()
		<-done
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() {
		_ = cli.Close()
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	return cli
}

func TestClientServerRoundTrip(t *testing.T) {
	var got RunArgs
	cli := startServer(t, testHandler{run: func(args RunArgs) RunReply {
		got = args
		return RunReply{
			ExitCode: 7,
			Output:   "playbook output",
			Facts:    map[string]json.RawMessage{"host": json.RawMessage(`{"ok":true}`)},
		}
	}})

	if err := cli.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	code, out, facts, err := cli.RunPlaybook(context.Background(), "home", "home/ping", "/work", "-m", "debug")
	if err != nil {
		t.Fatalf("RunPlaybook: %v", err)
	}
	wantArgs := RunArgs{
		Collection: "home",
		Scope:      "home/ping",
		WorkingDir: "/work",
		Args:       []string{"-m", "debug"},
	}
	if !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("args = %+v, want %+v", got, wantArgs)
	}
	if code != 7 || out != "playbook output" {
		t.Errorf("result = (%d, %q), want (7, %q)", code, out, "playbook output")
	}
	if string(facts["host"]) != `{"ok":true}` {
		t.Errorf("facts = %v, want host facts", facts)
	}
}

func TestRunPlaybookErrorPropagation(t *testing.T) {
	cli := startServer(t, testHandler{run: func(RunArgs) RunReply {
		return RunReply{ExitCode: 3, Output: "partial output", Error: "setup failed"}
	}})

	code, out, _, err := cli.RunPlaybook(context.Background(), "", "", ".")
	if err == nil || err.Error() != "setup failed" {
		t.Fatalf("RunPlaybook error = %v, want %q", err, "setup failed")
	}
	if code != 3 || out != "partial output" {
		t.Errorf("result = (%d, %q), want (3, %q)", code, out, "partial output")
	}
}

func TestRunPlaybookCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	cli := startServer(t, testHandler{run: func(RunArgs) RunReply {
		close(started)
		<-release
		return RunReply{Output: "finished after cancel"}
	}})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, _, err := cli.RunPlaybook(ctx, "", "", ".")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunPlaybook error = nil, want a cancellation error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunPlaybook did not return after cancellation")
	}

	close(release)
	// The late reply is discarded by net/rpc; the connection stays usable.
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := cli.Ping(context.Background())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Ping after cancellation: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConcurrentCalls(t *testing.T) {
	bothStarted := make(chan struct{})
	var mu sync.Mutex
	started := 0
	cli := startServer(t, testHandler{run: func(RunArgs) RunReply {
		mu.Lock()
		started++
		n := started
		mu.Unlock()
		if n == 2 {
			close(bothStarted)
		}
		<-bothStarted
		return RunReply{Output: "ok"}
	}})

	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, out, _, err := cli.RunPlaybook(context.Background(), "", "", ".")
			if err == nil && out != "ok" {
				err = &unexpectedOutput{out}
			}
			errs <- err
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent RunPlaybook: %v", err)
		}
	}
}

type unexpectedOutput struct{ out string }

func (e *unexpectedOutput) Error() string { return "unexpected output " + e.out }
