// Package ansible runs ansible-playbook inside the current process by calling
// the Ansible Python API through an embedded CPython interpreter. No
// ansible-playbook process is spawned.
package ansible

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	cpy "go.nhat.io/cpy/v3"

	"github.com/kakwa/strangle/internal/sshpool"
)

// setupScript defines the helpers used for capturing Python output and for
// clearing the global state that prevents Ansible's CLI from being run twice
// in the same interpreter.
//
// It also installs a guard around signal.signal. Ansible's TaskQueueManager
// replaces the process-wide SIGINT/SIGTERM handlers with its own during a
// run, clobbering the C-level handlers the host runtime (Go's os/signal)
// installed for signal delivery, and the Python API cannot restore arbitrary
// C handlers afterwards. Refusing to install handlers for those signals keeps
// SIGINT/SIGTERM delivery working for the life of the process.
const setupScript = `
import io as _strangle_io
import signal as _strangle_signal

_strangle_orig_signal = _strangle_signal.signal
_strangle_protected = (_strangle_signal.SIGINT, _strangle_signal.SIGTERM)

def _strangle_guarded_signal(signum, handler):
    if signum in _strangle_protected:
        return _strangle_signal.getsignal(signum)
    return _strangle_orig_signal(signum, handler)

_strangle_signal.signal = _strangle_guarded_signal

class _StrangleBuffer(_strangle_io.RawIOBase):
    def __init__(self):
        super().__init__()
        self._buf = _strangle_io.BytesIO()

    def writable(self):
        return True

    def write(self, data):
        return self._buf.write(data)

    def getvalue(self):
        return self._buf.getvalue().decode('utf-8', 'replace')

def _strangle_new_stdout():
    return _strangle_io.TextIOWrapper(_StrangleBuffer(), encoding='utf-8', errors='replace', line_buffering=True)

def _strangle_reset_ansible():
    import warnings
    warnings.filterwarnings('ignore', message='AnsibleCollectionFinder has already been configured')

    # Ansible caches parsed CLI args and vault secrets in process-wide
    # singletons. Clear them so each Run gets a fresh set of arguments.
    from ansible.parsing.vault import VaultSecretsContext
    from ansible.utils.context_objects import GlobalCLIArgs
    VaultSecretsContext._current = None
    GlobalCLIArgs._Singleton__instance = None

    # Ansible modules each create their own Display() instance at import
    # time, and every instance deduplicates messages for the life of the
    # process. Clear the memory of all live instances so messages repeated
    # from a new run are shown again.
    import sys as _strangle_sys
    from ansible.utils.display import Display as _strangle_Display
    for _mod in list(_strangle_sys.modules.values()):
        for _attr in ('display', '_display'):
            _d = getattr(_mod, _attr, None)
            if isinstance(_d, _strangle_Display):
                _d._errors.clear()
                _d._warns.clear()
                _d._deprecations.clear()
`

// Result is the outcome of a playbook run.
type Result struct {
	// ExitCode is the value returned by ansible-playbook (0 on success).
	ExitCode int
	// Output is everything Ansible wrote to stdout and stderr, including --diff.
	Output string
}

type request struct {
	args  []string
	dir   string
	reply chan response
}

type response struct {
	result Result
	err    error
}

// Runner owns a dedicated OS thread with an embedded CPython interpreter.
// All Python API calls happen on that thread, so the GIL never has to be
// handed between Go goroutines.
type Runner struct {
	requests  chan request
	closed    chan struct{}
	closeOnce sync.Once
	setup     string
	bridge    *sshpool.Bridge
	pluginDir string
}

// NewRunner initializes the embedded interpreter and starts its worker thread.
// A non-nil dispatcher enables the Go-managed SSH connection plugin.
func NewRunner(multi *sshpool.Multi) (*Runner, error) {
	r := &Runner{
		requests: make(chan request),
		closed:   make(chan struct{}),
		setup:    setupScript,
	}

	if multi != nil {
		dir, err := installPlugins()
		if err != nil {
			return nil, fmt.Errorf("install Ansible plugins: %w", err)
		}
		r.pluginDir = dir
		if err := os.Setenv("ANSIBLE_CONNECTION_PLUGINS", dir); err != nil {
			return nil, err
		}

		bridge, err := sshpool.NewBridge(multi)
		if err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		r.bridge = bridge
		r.setup = bridge.PythonSetup() + setupScript
	}

	ready := make(chan error, 1)
	go r.serve(ready)
	if err := <-ready; err != nil {
		r.Close()
		return nil, err
	}

	return r, nil
}

func (r *Runner) serve(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(r.closed)

	cpy.Py_InitializeEx(false) // leave Go's signal handlers in place
	defer cpy.Py_Finalize()

	if rc := cpy.PyRun_SimpleString(r.setup); rc != 0 {
		ready <- pythonError("ansible: failed to initialize Python")
		return
	}
	ready <- nil

	for req := range r.requests {
		result, err := run(req.dir, req.args)
		req.reply <- response{result: result, err: err}
	}
}

// Run invokes ansible-playbook with args in the embedded interpreter and
// returns its exit code and captured output.
func (r *Runner) Run(args ...string) (Result, error) {
	return r.RunDir("", args...)
}

// RunDir is like Run but changes the working directory of the embedded
// interpreter for the duration of the call. Ansible resolves ansible.cfg,
// relative playbook paths and roles relative to the working directory, so
// runs of a project's playbooks should use that project's directory.
func (r *Runner) RunDir(dir string, args ...string) (Result, error) {
	if len(args) == 0 {
		return Result{}, errors.New("ansible: no arguments given")
	}

	reply := make(chan response, 1)
	req := request{args: append([]string(nil), args...), dir: dir, reply: reply}

	select {
	case r.requests <- req:
	case <-r.closed:
		return Result{}, errors.New("ansible: runner is closed")
	}

	resp := <-reply
	return resp.result, resp.err
}

// Close shuts down the embedded interpreter. It is safe to call more than once.
func (r *Runner) Close() {
	r.closeOnce.Do(func() {
		close(r.requests)
		<-r.closed
		if r.bridge != nil {
			r.bridge.Close()
		}
		if r.pluginDir != "" {
			_ = os.RemoveAll(r.pluginDir)
		}
	})
}

func run(dir string, args []string) (result Result, err error) {
	if rc := cpy.PyRun_SimpleString("_strangle_reset_ansible()"); rc != 0 {
		return Result{}, pythonError("ansible: failed to reset state")
	}

	if dir != "" {
		snippet := "import os as _strangle_os\n" +
			"_strangle_cwd = _strangle_os.getcwd()\n" +
			"_strangle_os.chdir(" + pyString(dir) + ")"
		if rc := cpy.PyRun_SimpleString(snippet); rc != 0 {
			return Result{}, pythonError("ansible: failed to change working directory")
		}
		defer func() {
			_ = cpy.PyRun_SimpleString("_strangle_os.chdir(_strangle_cwd)")
		}()
	}

	// The ansible CLI parses sys.argv[1:] and treats sys.argv[0] as the
	// program name, so the list must start with one; otherwise the first
	// real argument (e.g. --diff) is silently dropped.
	argv := cpy.PyList_New(len(args) + 1)
	defer argv.DecRef()
	if cpy.PyList_SetItem(argv, 0, cpy.PyUnicode_FromString("ansible-playbook")) != 0 {
		return Result{}, pythonError("ansible: failed to build argv")
	}
	for i, arg := range args {
		if cpy.PyList_SetItem(argv, i+1, cpy.PyUnicode_FromString(arg)) != 0 {
			return Result{}, pythonError("ansible: failed to build argv")
		}
	}
	cpy.PySys_SetObject("argv", argv)

	mainMod := cpy.PyImport_AddModule("__main__")
	factory := mainMod.GetAttrString("_strangle_new_stdout")
	if factory == nil {
		return Result{}, pythonError("ansible: missing output helper")
	}
	buf := factory.CallObject(nil)
	factory.DecRef()
	if buf == nil {
		return Result{}, pythonError("ansible: failed to create output buffer")
	}

	oldOut := cpy.PySys_GetObject("stdout")
	oldErr := cpy.PySys_GetObject("stderr")
	cpy.PySys_SetObject("stdout", buf)
	cpy.PySys_SetObject("stderr", buf)
	defer func() {
		cpy.PySys_SetObject("stdout", oldOut)
		cpy.PySys_SetObject("stderr", oldErr)
		buf.DecRef()
	}()

	playbook := cpy.PyImport_ImportModule("ansible.cli.playbook")
	if playbook == nil {
		return Result{}, pythonError("ansible: failed to import ansible.cli.playbook")
	}
	defer playbook.DecRef()

	main := playbook.GetAttrString("main")
	if main == nil {
		return Result{}, pythonError("ansible: cli entrypoint not found")
	}
	defer main.DecRef()

	ret := main.CallObject(nil)
	if ret == nil {
		if !cpy.PyErr_ExceptionMatches(cpy.PyExc_SystemExit) {
			return Result{}, pythonError("ansible: playbook run failed")
		}

		exc := cpy.PyErr_GetRaisedException()
		if exc != nil {
			code := exc.GetAttrString("code")
			switch {
			case cpy.PyLong_Check(code):
				result.ExitCode = cpy.PyLong_AsLong(code)
			case code == nil || code == cpy.Py_None:
				result.ExitCode = 0
			default:
				result.ExitCode = 1
				err = fmt.Errorf("ansible: SystemExit(%s)", cpy.PyUnicode_AsUTF8(code.Str()))
			}
			decRef(code)
			exc.DecRef()
		}
		cpy.PyErr_Clear()
	} else {
		if cpy.PyLong_Check(ret) {
			result.ExitCode = cpy.PyLong_AsLong(ret)
		}
		ret.DecRef()
	}

	buf.CallMethodArgs("flush")
	inner := buf.GetAttrString("buffer")
	value := inner.CallMethodArgs("getvalue")
	if value != nil {
		result.Output = strings.TrimSpace(cpy.PyUnicode_AsUTF8(value))
		value.DecRef()
	}
	decRef(inner)

	return result, err
}

// pyString renders s as a single-quoted Python string literal.
func pyString(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'':
			b.WriteString(`\'`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

func pythonError(what string) error {
	exc := cpy.PyErr_GetRaisedException()
	if exc == nil {
		cpy.PyErr_Clear()
		return errors.New(what)
	}
	defer exc.DecRef()

	text := exc.Str()
	defer decRef(text)

	cpy.PyErr_Clear()
	if text == nil {
		return errors.New(what)
	}
	return fmt.Errorf("%s: %s", what, cpy.PyUnicode_AsUTF8(text))
}

func decRef(obj *cpy.PyObject) {
	if obj != nil {
		obj.DecRef()
	}
}
