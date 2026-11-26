package model

import "time"

// RunRecord is one playbook execution. It is both the row of the aggregate
// runs table and the record the Store exposes; per-host results live in
// ansible_runs, the aggregate is kept for the run listing APIs.
type RunRecord struct {
	ID           uint64    `gorm:"primaryKey" json:"id"`
	Target       string    `gorm:"size:255;index" json:"target"`
	Playbook     string    `gorm:"size:512;index" json:"playbook"`
	StartedAt    time.Time `json:"started_at"`
	FinishedAt   time.Time `gorm:"index" json:"finished_at"`
	DurationMS   int64     `json:"duration_ms"`
	ExitCode     int       `json:"exit_code"`
	Success      bool      `json:"success"`
	Hosts        int       `json:"hosts"`
	ChangedHosts int       `json:"changed_hosts"`
	FailedHosts  int       `json:"failed_hosts"`
	Output       string    `gorm:"type:text" json:"output,omitempty"`
	State        string    `gorm:"size:32;index" json:"state,omitempty"`
	// Canceled records when cancellation was requested for the run.
	Canceled bool `json:"canceled,omitempty"`
	// Check records a dry run triggered with --check.
	Check bool `json:"check,omitempty"`
	// JobID is the queue job that executes this run, when available.
	JobID string `gorm:"size:64;index" json:"job_id,omitempty"`
	// Inventory is the inventory file/path used by this run. Together with
	// Target and Playbook it identifies a host's context.
	Inventory string `gorm:"size:1024" json:"inventory,omitempty"`
}

func (RunRecord) TableName() string { return "runs" }
