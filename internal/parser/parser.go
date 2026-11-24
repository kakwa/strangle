// Package parser extracts per-host and per-task results from the captured
// output of an ansible-playbook --diff run.
package parser

import (
	"regexp"
	"strconv"
	"strings"
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

// HostSummary is the per-host recap line.
type HostSummary struct {
	Host        string
	Ok          int
	Changed     int
	Failed      int
	Unreachable int
	Skipped     int
	Rescued     int
	Ignored     int
}

// Status derives a host's overall status from its recap counts: failed and
// unreachable win over changed, which wins over ok.
func Status(ok, changed, failed, unreachable int) string {
	switch {
	case failed > 0:
		return "failed"
	case unreachable > 0:
		return "unreachable"
	case changed > 0:
		return "changed"
	default:
		return "ok"
	}
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
		currentTask   string
		inRecap       bool
		inJSON        bool
		collectingDiff bool
		pendingDiff   []string
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

		// JSON continuation after "result: [host] => {".
		if inJSON {
			if strings.TrimSpace(line) == "}" || strings.HasSuffix(strings.TrimSpace(line), "}") {
				// A closing brace ends the JSON; a line that is only the
				// closing brace ends the block.
				if strings.TrimSpace(line) == "}" {
					inJSON = false
				}
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
				s := HostSummary{Host: m[1]}
				for i, field := range []string{m[2], m[3], m[4], m[5], m[6], m[7], m[8]} {
					n, _ := strconv.Atoi(field)
					switch i {
					case 0:
						s.Ok = n
					case 1:
						s.Changed = n
					case 2:
						s.Unreachable = n
					case 3:
						s.Failed = n
					case 4:
						s.Skipped = n
					case 5:
						s.Rescued = n
					case 6:
						s.Ignored = n
					}
				}
				summaries = append(summaries, s)
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
				currentTask = strings.TrimSpace(trimmed[len("TASK [") : end])
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
			if c == '\\' {
				i++
			} else if c == '"' {
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

func unquoteJSONString(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
