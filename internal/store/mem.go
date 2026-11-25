package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/kakwa/strangle/internal/parser"
)

// Mem is an in-memory Store, used when no database DSN is configured and in
// tests.
type Mem struct {
	mu     sync.RWMutex
	nextID uint64
	runs   []RunRecord
	hosts  map[string]*HostRecord
	// hostRuns keeps recap lines denormalized with the host name.
	hostRuns []HostRunRecord
	tasks    map[string]*TaskStateRecord // key: host + "\x00" + task
	// taskRuns is append-only history: one row per task result per run.
	taskRuns []TaskRunRecord
}

// NewMem returns an empty in-memory Store.
func NewMem() *Mem {
	return &Mem{
		hosts: map[string]*HostRecord{},
		tasks: map[string]*TaskStateRecord{},
	}
}

func (s *Mem) SaveRun(_ context.Context, in RunInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if in.Run.ID == 0 {
		s.nextID++
		in.Run.ID = s.nextID
	} else {
		if in.Run.ID >= s.nextID {
			s.nextID = in.Run.ID + 1
		}
	}
	run := in.Run
	s.runs = append(s.runs, run)

	target := in.Run.Target

	seenInRecap := map[string]HostRunRecord{}
	for _, h := range in.Hosts {
		seenInRecap[h.Host] = h
	}

	// Inventory snapshot.
	for _, ih := range in.Inventory {
		host := s.host(target, ih.Name, run)
		host.IP = ih.IP
		host.User = ih.User
		host.Groups = ih.Groups
		host.InInventory = true
	}

	// Recap lines: hosts that answered this run.
	for _, h := range in.Hosts {
		host := s.host(target, h.Host, run)
		host.LastSeenAt = run.FinishedAt
		host.LastRunAt = run.FinishedAt
		host.LastStatus = parser.Status(h.Ok, h.Changed, h.Failed, h.Unreachable)
		host.Active = true
		host.DisappearedAt = nil
		if facts := in.Facts[h.Host]; len(facts) > 0 {
			host.Facts = append(json.RawMessage(nil), facts...)
		}

		rec := h
		rec.RunID = run.ID
		rec.Target = target
		rec.Playbook = run.Playbook
		rec.StartedAt = run.StartedAt
		rec.DurationMS = run.DurationMS
		rec.ExitCode = run.ExitCode
		rec.Success = run.Success
		s.hostRuns = append(s.hostRuns, rec)
	}

	// Scope hosts that did not answer are disappeared.
	for _, name := range in.ScopeNames {
		if _, ok := seenInRecap[name]; ok {
			continue
		}
		host := s.host(target, name, run)
		host.Active = false
		if host.DisappearedAt == nil {
			t := run.FinishedAt
			host.DisappearedAt = &t
		}
	}

	// Task states and per-run history.
	hostChanged := map[string]bool{}
	for _, t := range in.Tasks {
		s.taskRuns = append(s.taskRuns, TaskRunRecord{
			ID:         uint64(len(s.taskRuns) + 1),
			RunID:      run.ID,
			Target:     target,
			Host:       t.Host,
			Task:       t.Task,
			Status:     t.Status,
			Diff:       t.LastDiff,
			Error:      t.LastError,
			FinishedAt: run.FinishedAt,
		})
		key := target + "\x00" + t.Host + "\x00" + t.Task
		ts := s.tasks[key]
		if ts == nil {
			ts = &TaskStateRecord{}
			s.tasks[key] = ts
		}
		ts.Target = target
		ts.Host = t.Host
		ts.Task = t.Task
		ts.Status = t.Status
		ts.LastRunID = run.ID
		ts.LastRunAt = run.FinishedAt
		if t.Status == "changed" {
			changedAt := run.FinishedAt
			ts.LastChangedAt = &changedAt
			ts.LastDiff = t.LastDiff
			hostChanged[t.Host] = true
		}
		if t.Status == "failed" {
			ts.LastError = t.LastError
		}
	}
	for host := range hostChanged {
		if h, ok := s.hosts[target+"\x00"+host]; ok {
			t := run.FinishedAt
			h.LastChangeAt = &t
		}
	}

	return nil
}

// host returns the host record for a target, creating it on first sight.
func (s *Mem) host(target, name string, run RunRecord) *HostRecord {
	key := target + "\x00" + name
	h, ok := s.hosts[key]
	if !ok {
		h = &HostRecord{Target: target, Name: name, FirstSeenAt: run.FinishedAt}
		s.hosts[key] = h
	}
	return h
}

func (s *Mem) ListRuns(_ context.Context, limit int) ([]RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]RunRecord, 0, min(limit, len(s.runs)))
	for i := len(s.runs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.runs[i])
	}
	return out, nil
}

func (s *Mem) Run(_ context.Context, id uint64) (RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for i := len(s.runs) - 1; i >= 0; i-- {
		if s.runs[i].ID == id {
			return s.runs[i], nil
		}
	}
	return RunRecord{}, fmt.Errorf("run %d: %w", id, ErrNotFound)
}

func (s *Mem) RunTasks(_ context.Context, runID uint64) ([]TaskRunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	found := false
	for _, run := range s.runs {
		if run.ID == runID {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("run %d: %w", runID, ErrNotFound)
	}
	out := make([]TaskRunRecord, 0)
	for _, t := range s.taskRuns {
		if t.RunID == runID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *Mem) ListHosts(_ context.Context) ([]HostRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]HostRecord, 0, len(s.hosts))
	for _, h := range s.hosts {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *Mem) HostRuns(_ context.Context, host string, limit int) ([]HostRunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]HostRunRecord, 0)
	for i := len(s.hostRuns) - 1; i >= 0; i-- {
		if s.hostRuns[i].Host == host {
			out = append(out, s.hostRuns[i])
		}
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *Mem) HostChanges(_ context.Context, host string, limit int) ([]TaskStateRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]TaskStateRecord, 0)
	for _, ts := range s.tasks {
		if ts.Host == host {
			out = append(out, *ts)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].LastChangedAt, out[j].LastChangedAt
		if (a == nil) != (b == nil) {
			return a != nil
		}
		if a != nil {
			if !a.Equal(*b) {
				return a.After(*b)
			}
		}
		return out[i].LastRunAt.After(out[j].LastRunAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Mem) Stats(_ context.Context) (*Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := &Stats{PerPlaybook: []PlaybookStats{}}

	for _, h := range s.hosts {
		st.Hosts.Total++
		if h.InInventory {
			st.Hosts.InInventory++
		}
		if h.Active {
			st.Hosts.Active++
		} else if h.InInventory {
			st.Hosts.Disappeared++
		}
	}

	var totalDur int64
	perPB := map[string]*PlaybookStats{}
	for i := range s.runs {
		r := &s.runs[i]
		st.Runs.Total++
		if r.Success {
			st.Runs.Succeeded++
		} else {
			st.Runs.Failed++
		}
		totalDur += r.DurationMS
		if r.FinishedAt.After(st.Runs.LastRunAt) {
			st.Runs.LastRunAt = r.FinishedAt
		}
		key := r.Target + "\x00" + r.Playbook
		pb := perPB[key]
		if pb == nil {
			pb = &PlaybookStats{Target: r.Target, Playbook: r.Playbook}
			perPB[key] = pb
		}
		pb.Total++
		if !r.Success {
			pb.Failed++
		}
		if r.FinishedAt.After(pb.LastRunAt) {
			pb.LastRunAt = r.FinishedAt
		}
	}
	if st.Runs.Total > 0 {
		st.Runs.AvgDurationMS = totalDur / int64(st.Runs.Total)
	}
	for _, pb := range perPB {
		st.PerPlaybook = append(st.PerPlaybook, *pb)
	}
	sort.Slice(st.PerPlaybook, func(i, j int) bool {
		if st.PerPlaybook[i].Target != st.PerPlaybook[j].Target {
			return st.PerPlaybook[i].Target < st.PerPlaybook[j].Target
		}
		return st.PerPlaybook[i].Playbook < st.PerPlaybook[j].Playbook
	})

	return st, nil
}

func (s *Mem) Close() error { return nil }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
