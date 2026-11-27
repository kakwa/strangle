package ssh

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type bridgeRequest struct {
	ID       int      `json:"id"`
	Op       string   `json:"op"`
	Endpoint Endpoint `json:"endpoint"`
	Command  string   `json:"command,omitempty"`
	Stdin    string   `json:"stdin,omitempty"`
	Path     string   `json:"path,omitempty"`
	Content  string   `json:"content,omitempty"`
	Mode     uint32   `json:"mode,omitempty"`
}

type bridgeResponse struct {
	ID     int             `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type execResponse struct {
	ReturnCode int    `json:"return_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
}

type fetchResponse struct {
	Content string `json:"content"`
}

// Bridge connects the (possibly forked) Ansible Python workers to the Go SSH
// managers using inherited pipes. No Python code calls back into cgo.
type Bridge struct {
	multi *Multi

	reqR  *os.File
	reqW  *os.File
	respR *os.File
	respW *os.File
	lock  *os.File
}

// NewBridge creates the pipes used by the Ansible connection plugin and starts
// serving requests. The returned fds are inherited by Ansible's forked workers.
func NewBridge(multi *Multi) (*Bridge, error) {
	if multi == nil {
		return nil, fmt.Errorf("ssh bridge requires an SSH dispatcher")
	}

	reqFDs, err := blockingPipe()
	if err != nil {
		return nil, err
	}
	respFDs, err := blockingPipe()
	if err != nil {
		return nil, err
	}
	lock, err := os.CreateTemp("", "strangle-ssh-lock-*")
	if err != nil {
		return nil, err
	}

	b := &Bridge{
		multi: multi,
		reqR:  os.NewFile(uintptr(reqFDs[0]), "strangle-ssh-req-r"),
		reqW:  os.NewFile(uintptr(reqFDs[1]), "strangle-ssh-req-w"),
		respR: os.NewFile(uintptr(respFDs[0]), "strangle-ssh-resp-r"),
		respW: os.NewFile(uintptr(respFDs[1]), "strangle-ssh-resp-w"),
		lock:  lock,
	}
	go b.serve()
	return b, nil
}

// PythonSetup declares the fd numbers for the embedded Python interpreter.
// The lock path is passed (not an fd) because forked workers need to open
// their own file description for flock to serialize between them; an inherited
// fd would share one lock across all processes.
func (b *Bridge) PythonSetup() string {
	return fmt.Sprintf(
		"import builtins\nbuiltins._strangle_ssh_fds = {'req': %d, 'resp': %d, 'lock_path': %q}\n",
		b.reqW.Fd(), b.respR.Fd(), b.lock.Name(),
	)
}

// Close stops the bridge and releases its descriptors.
func (b *Bridge) Close() {
	_ = b.reqR.Close()
	_ = b.reqW.Close()
	_ = b.respR.Close()
	_ = b.respW.Close()
	_ = b.lock.Close()
	_ = os.Remove(b.lock.Name())
}

func (b *Bridge) serve() {
	reader := bufio.NewReader(b.reqR)
	writer := bufio.NewWriter(b.respW)
	defer writer.Flush() //nolint:errcheck // best-effort flush at bridge teardown

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		var req bridgeRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}

		resp := b.handle(req)
		data, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		if _, err := writer.Write(append(data, '\n')); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
	}
}

func (b *Bridge) handle(req bridgeRequest) bridgeResponse {
	resp := bridgeResponse{ID: req.ID}
	switch req.Op {
	case "exec":
		stdin, err := base64.StdEncoding.DecodeString(req.Stdin)
		if err != nil {
			resp.Error = fmt.Sprintf("decode stdin: %v", err)
			return resp
		}
		result, err := b.multi.Exec(req.Endpoint, req.Command, stdin)
		if err != nil {
			resp.Error = err.Error()
			return resp
		}
		payload, _ := json.Marshal(execResponse{
			ReturnCode: result.ReturnCode,
			Stdout:     base64.StdEncoding.EncodeToString(result.Stdout),
			Stderr:     base64.StdEncoding.EncodeToString(result.Stderr),
		})
		resp.OK, resp.Result = true, payload
	case "put":
		content, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			resp.Error = fmt.Sprintf("decode content: %v", err)
			return resp
		}
		if err := b.multi.Put(req.Endpoint, req.Path, content, req.Mode); err != nil {
			resp.Error = err.Error()
			return resp
		}
		resp.OK = true
	case "fetch":
		content, err := b.multi.Fetch(req.Endpoint, req.Path)
		if err != nil {
			resp.Error = err.Error()
			return resp
		}
		payload, _ := json.Marshal(fetchResponse{Content: base64.StdEncoding.EncodeToString(content)})
		resp.OK, resp.Result = true, payload
	default:
		resp.Error = fmt.Sprintf("unknown operation %q", req.Op)
	}
	return resp
}

func blockingPipe() ([2]int, error) {
	var fds [2]int
	return fds, unix.Pipe2(fds[:], 0)
}
