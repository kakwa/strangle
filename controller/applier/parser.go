// Parsing of captured ansible-playbook output lives here alongside the
// applier that records it.
package applier

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/kakwa/strangle/model"
)

// TaskResult is the outcome of one task on one host.
type TaskResult struct {
	// Task is the task name from the "TASK [name]" header.
	Task string
	// Host is the inventory hostname.
	Host string
	// Status is ok, changed, failed, skipped or unreachable.
	Status string
	// Diff holds the --diff block for this task on this host (changed/failed only).
	Diff string
	// Error holds the "msg" of a failed task, when present.
	Error string
}

// HostSummary is the per-host recap line; the counters are the model's
// shared TaskCounters.
type HostSummary struct {
	Host string
	model.TaskCounters
}

var (
	// ANSI color codes (kept as a safety net; the captured output is usually
	// colorless because stdout is not a TTY).
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

	// Result lines look like:
	//   ok: [host]
	//   changed: [host]
	//   fatal: [host]: FAILED! => {...}
	//   skipping: [host]
	//   unreachable: [host]
	resultRE = regexp.MustCompile(`^(ok|changed|failed|fatal|skipping|skipped|unreachable|rescued|ignored|CRITICAL)\s*:\s*\[([^\]]+)\]`)

	recapRE = regexp.MustCompile(
		`^(\S+)\s*:\s*ok=(\d+)\s+changed=(\d+)\s+unreachable=(\d+)\s+failed=(\d+)\s+skipped=(\d+)\s+rescued=(\d+)\s+ignored=(\d+)`)

	msgRE = regexp.MustCompile(`"msg":\s*"((?:[^"\\]|\\.)*)"`)
)

// Parse walks the playbook output and returns the recap lines and the
// per-task results.
func Parse(output string) (summaries []HostSummary, tasks []TaskResult) {
	lines := strings.Split(ansiRE.ReplaceAllString(output, ""), "\n")

	var (
		currentTask    string
		inRecap        bool
		inJSON         bool
		collectingDiff bool
		pendingDiff    []string
	)

	flushDiff := func(tr *TaskResult) {
		if len(pendingDiff) > 0 && (tr.Status == "changed" || tr.Status == "failed") {
			tr.Diff = strings.TrimRight(strings.Join(pendingDiff, "\n"), "\n")
		}
		pendingDiff = nil
		collectingDiff = false
	}

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimLeft(line, " ")

		// JSON continuation after "result: [host] => {". Only a line that
		// is just the closing brace ends the block.
		if inJSON {
			if strings.TrimSpace(line) == "}" {
				inJSON = false
			}
			continue
		}

		if strings.HasPrefix(trimmed, "PLAY RECAP") {
			inRecap = true
			collectingDiff = false
			pendingDiff = nil
			continue
		}
		if inRecap {
			if m := recapRE.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				summaries = append(summaries, HostSummary{
					Host: m[1],
					TaskCounters: model.TaskCounters{
						Ok: atoi(m[2]), Changed: atoi(m[3]), Unreachable: atoi(m[4]),
						Failed: atoi(m[5]), Skipped: atoi(m[6]), Rescued: atoi(m[7]), Ignored: atoi(m[8]),
					},
				})
			}
			continue
		}

		if strings.HasPrefix(trimmed, "PLAY [") {
			currentTask = ""
			collectingDiff = false
			pendingDiff = nil
			continue
		}
		if strings.HasPrefix(trimmed, "TASK [") {
			if end := strings.Index(trimmed, "]"); end > 0 {
				currentTask = strings.TrimSpace(trimmed[len("TASK ["):end])
			} else {
				currentTask = ""
			}
			collectingDiff = false
			pendingDiff = nil
			continue
		}

		if m := resultRE.FindStringSubmatch(trimmed); m != nil {
			status := m[1]
			switch status {
			case "fatal", "failed", "CRITICAL":
				status = "failed"
			case "skipping", "skipped":
				status = "skipped"
			}
			tr := TaskResult{Task: currentTask, Host: m[2], Status: status}
			if status == "failed" {
				if mm := msgRE.FindStringSubmatch(line); mm != nil {
					tr.Error = unquoteJSONString(mm[1])
				}
			}
			flushDiff(&tr)
			// Multi-line JSON body after "=> {".
			if strings.Contains(line, "=> {") && !jsonBalanced(line) {
				inJSON = true
			}
			tasks = append(tasks, tr)
			continue
		}

		// Diff block: starts at "--- ", "+++ " or "@@ " and runs until the
		// next result or structural line. Diff lines may be indented (context)
		// or start with +/-/@.
		if strings.HasPrefix(trimmed, "--- ") || strings.HasPrefix(trimmed, "+++ ") || strings.HasPrefix(trimmed, "@@ ") {
			collectingDiff = true
			pendingDiff = append(pendingDiff, line)
			continue
		}
		if collectingDiff {
			pendingDiff = append(pendingDiff, line)
			continue
		}
	}
	return summaries, tasks
}

// jsonBalanced reports whether all braces in the line are balanced, i.e. the
// JSON value completed on this line.
func jsonBalanced(line string) bool {
	depth := 0
	inStr := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		if inStr {
			switch c {
			case '\\':
				i++
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	return depth <= 0 && !inStr
}

// atoi is strconv.Atoi that drops the error: recap fields are always
// well-formed digits.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// unquoteJSONString decodes the escaped contents of a JSON string literal
// (the capture of "msg"). If the capture is not valid JSON the raw text is
// returned unchanged.
func unquoteJSONString(s string) string {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return s
	}
	return out
}
