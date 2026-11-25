// Package store defines the persistence interface for strangle: run records,
// per-host run results, per-task change traces and aggregate stats.
// Implementations live in store (in-memory) and db (gorm/PostgreSQL).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned by Store lookups when no record has that ID.
var ErrNotFound = errors.New("record not found")

// Run states recorded while a run is in flight. Records without a state are
// completed runs from earlier versions.
const (
	RunStateQueued    = "queued"
	RunStateRunning   = "running"
	RunStateCompleted = "completed"
	RunStateCancelled = "cancelled"
)

// RunRecord is one playbook execution.
type RunRecord struct {
	ID           uint64    `json:"id"`
	Target       string    `json:"target"`
	Playbook     string    `json:"playbook"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `json:"finished_at"`
	DurationMS   int64     `json:"duration_ms"`
	ExitCode     int       `json:"exit_code"`
	Success      bool      `json:"success"`
	Hosts        int       `json:"hosts"`
	ChangedHosts int       `json:"changed_hosts"`
	FailedHosts  int       `json:"failed_hosts"`
	Output       string    `json:"output,omitempty"`
	State        string    `json:"state,omitempty"`
	// Canceled records when cancellation was requested for the run.
	Canceled bool `json:"canceled,omitempty"`
	// Check records a dry run triggered with --check.
	Check bool `json:"check,omitempty"`
	// JobID is the queue job that executes this run, when available.
	JobID string `json:"job_id,omitempty"`
}

// HostRecord is a host's current state, scoped to the target it appears in.
type HostRecord struct {
	Target        string     `json:"target"`
	Name          string     `json:"name"`
	IP            string     `json:"ip,omitempty"`
	User          string     `json:"user,omitempty"`
	Groups        []string   `json:"groups,omitempty"`
	InInventory   bool       `json:"in_inventory"`
	Active        bool       `json:"active"`
	FirstSeenAt   time.Time  `json:"first_seen_at"`
	LastSeenAt    time.Time  `json:"last_seen_at"`
	LastRunAt     time.Time  `json:"last_run_at"`
	LastStatus    string     `json:"last_status"` // ok | changed | failed | unreachable | ""
	LastChangeAt  *time.Time `json:"last_change_at,omitempty"`
	DisappearedAt *time.Time `json:"disappeared_at,omitempty"`
	// Facts is the latest ansible_facts blob collected for this host. It is
	// replaced only by a later run that actually returned facts.
	Facts json.RawMessage `json:"facts,omitempty"`
}

// HostRunRecord is a host's recap line for one run, joined with the run.
type HostRunRecord struct {
	RunID       uint64    `json:"run_id"`
	Target      string    `json:"target"`
	Host        string    `json:"host"`
	Playbook    string    `json:"playbook"`
	StartedAt   time.Time `json:"started_at"`
	DurationMS  int64     `json:"duration_ms"`
	ExitCode    int       `json:"exit_code"`
	Success     bool      `json:"success"`
	Status      string    `json:"status"`
	Ok          int       `json:"ok"`
	Changed     int       `json:"changed"`
	Failed      int       `json:"failed"`
	Unreachable int       `json:"unreachable"`
	Skipped     int       `json:"skipped"`
	Rescued     int       `json:"rescued"`
	Ignored     int       `json:"ignored"`
}

// TaskStateRecord is the latest known state of one task on one host.
type TaskStateRecord struct {
	Target        string     `json:"target"`
	Host          string     `json:"host"`
	Task          string     `json:"task"`
	Status        string     `json:"status"`
	LastRunID     uint64     `json:"last_run_id"`
	LastRunAt     time.Time  `json:"last_run_at"`
	LastChangedAt *time.Time `json:"last_changed_at,omitempty"`
	LastDiff      string     `json:"last_diff,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// TaskRunRecord is one task result from one run. Unlike TaskStateRecord (the
// latest state of a host/task), these rows are append-only: every run keeps
// its own result, including repeated task names and diffs/errors.
type TaskRunRecord struct {
	ID         uint64    `json:"id"`
	RunID      uint64    `json:"run_id"`
	Target     string    `json:"target"`
	Host       string    `json:"host"`
	Task       string    `json:"task"`
	Status     string    `json:"status"`
	Diff       string    `json:"diff,omitempty"`
	Error      string    `json:"error,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// PlaybookStats is the per-(target, playbook) run breakdown in Stats.
type PlaybookStats struct {
	Target    string    `json:"target"`
	Playbook  string    `json:"playbook"`
	Total     int       `json:"total"`
	Failed    int       `json:"failed"`
	LastRunAt time.Time `json:"last_run_at"`
}

// HostsStats is the host breakdown in Stats.
type HostsStats struct {
	Total       int `json:"total"`
	InInventory int `json:"in_inventory"`
	Active      int `json:"active"`
	Disappeared int `json:"disappeared"`
}

// RunsStats is the run breakdown in Stats.
type RunsStats struct {
	Total         int       `json:"total"`
	Succeeded     int       `json:"succeeded"`
	Failed        int       `json:"failed"`
	AvgDurationMS int64     `json:"avg_duration_ms"`
	LastRunAt     time.Time `json:"last_run_at"`
}

// Stats is the aggregate view returned by Store.Stats.
type Stats struct {
	Hosts       HostsStats      `json:"hosts"`
	Runs        RunsStats       `json:"runs"`
	PerPlaybook []PlaybookStats `json:"per_playbook"`
}

// RunInput is everything a Store needs to persist one playbook run.
type RunInput struct {
	// Run is the run to insert.
	Run RunRecord
	// Hosts are the per-host recap lines of this run.
	Hosts []HostRunRecord
	// Tasks are the per-(host, task) results observed in this run. Stores
	// update their latest-state view from these and append one history row
	// per result.
	Tasks []TaskStateRecord
	// Facts maps each host to its latest ansible_facts blob from this run.
	// Hosts without facts keep whatever was stored by earlier runs.
	Facts map[string]json.RawMessage
	// ScopeNames are the hosts this run was expected to reach (the play
	// targets resolved against the inventory). Hosts in the scope but absent
	// from the recap are marked as disappeared.
	ScopeNames []string
	// Inventory is the current inventory snapshot (hosts, IPs, groups).
	Inventory []InventoryHost
}

// InventoryHost is a host from the parsed inventory file.
type InventoryHost struct {
	Name   string   `json:"name"`
	IP     string   `json:"ip,omitempty"`
	User   string   `json:"user,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

// RetentionPolicy describes which finished runs of one collection may be
// purged. MaxAge deletes runs older than the duration; MaxCount keeps only
// the newest N runs. A zero field disables that rule; both zero disables
// retention for the collection.
type RetentionPolicy struct {
	MaxAge   time.Duration
	MaxCount int
}

// Enabled reports whether either retention rule is configured.
func (p RetentionPolicy) Enabled() bool {
	return p.MaxAge > 0 || p.MaxCount > 0
}

// PruneResult counts the rows removed by one retention pass.
type PruneResult struct {
	Runs        int64
	HostRuns    int64
	TaskResults int64
}

// Store persists and queries run history, host state and task change traces.
type Store interface {
	// CreateRun reserves an ID for a run that has not finished yet. Callers
	// pass a record with Target and Playbook set; the store fills in ID and
	// started_at. It is idempotent when ID is already set.
	CreateRun(ctx context.Context, in *RunRecord) error
	// SaveRun atomically persists one playbook run and updates host state.
	SaveRun(ctx context.Context, in RunInput) error
	// UpdateRun updates the mutable fields of an existing run (state, job ID,
	// output, result). It returns store.ErrNotFound for an unknown ID.
	UpdateRun(ctx context.Context, in RunRecord) error
	// ListRuns returns up to limit runs, newest first. Run summaries need not
	// carry the full captured output.
	ListRuns(ctx context.Context, limit int) ([]RunRecord, error)
	// Run returns one run by ID, including its full captured output.
	Run(ctx context.Context, id uint64) (RunRecord, error)
	// ListHosts returns all known hosts.
	ListHosts(ctx context.Context) ([]HostRecord, error)
	// HostRuns returns up to limit of the host's runs (across all targets),
	// newest first.
	HostRuns(ctx context.Context, host string, limit int) ([]HostRunRecord, error)
	// HostChanges returns the host's task change traces (across all targets),
	// most recently changed first.
	HostChanges(ctx context.Context, host string, limit int) ([]TaskStateRecord, error)
	// RunTasks returns every per-task result recorded for one run, in the
	// order the tasks were parsed from that run's output.
	RunTasks(ctx context.Context, runID uint64) ([]TaskRunRecord, error)
	// PruneRuns deletes finished runs of one collection according to the
	// policy. Queued and running runs are never removed. It also removes the
	// per-run host and task rows that would otherwise become orphans.
	PruneRuns(ctx context.Context, target string, policy RetentionPolicy) (PruneResult, error)
	// PruneHosts deletes hosts of one collection that are no longer in its
	// inventory and have not been seen for delay. It returns the number of
	// hosts removed and deletes their per-host history rows with them.
	PruneHosts(ctx context.Context, target string, delay time.Duration) (int64, error)
	// Stats returns aggregate statistics.
	Stats(ctx context.Context) (*Stats, error)
	// Close releases underlying resources.
	Close() error
}
