// Master/worker JSON-RPC protocol. It uses only the standard library
// (net/rpc with the net/rpc/jsonrpc codec): no schema or generated code.
package worker

import "encoding/json"

const (
	serviceName = "Worker"
	pingMethod  = serviceName + ".Ping"
	runMethod   = serviceName + ".RunPlaybook"
)

// PingArgs and PingReply are the request/response of the liveness method.
type PingArgs struct{}

type PingReply struct {
	OK bool `json:"ok"`
}

// RunArgs carries one playbook dispatch.
type RunArgs struct {
	Collection string   `json:"collection,omitempty"`
	Scope      string   `json:"scope,omitempty"`
	WorkingDir string   `json:"working_dir"`
	Args       []string `json:"args,omitempty"`
}

// RunReply carries the exit code, captured output and per-host facts.
type RunReply struct {
	ExitCode int                        `json:"exit_code"`
	Output   string                     `json:"output"`
	Error    string                     `json:"error,omitempty"`
	Facts    map[string]json.RawMessage `json:"facts,omitempty"`
}
