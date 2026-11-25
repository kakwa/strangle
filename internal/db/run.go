package db

import "time"

// Run is the aggregate row for one playbook execution. Per-host results live
// in ansible_runs; the aggregate is kept for the run listing APIs.
type Run struct {
	ID           uint64 `gorm:"primaryKey"`
	Target       string `gorm:"size:255;index"`
	Playbook     string `gorm:"size:512;index"`
	StartedAt    time.Time
	FinishedAt   time.Time `gorm:"index"`
	DurationMS   int64
	ExitCode     int
	Success      bool
	Hosts        int
	ChangedHosts int
	FailedHosts  int
	Output       string `gorm:"type:text"`
	State        string `gorm:"size:32;index"`
	Canceled     bool
	Check        bool
	JobID        string `gorm:"size:64;index"`
	Inventory    string `gorm:"size:1024"`
}
