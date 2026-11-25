package db

import "time"

// TaskRunResult is one task result of one run. It is append-only:
// task_states keeps only the latest state per host/task, while this table
// preserves every run's status, diff and error (including repeated names).
type TaskRunResult struct {
	ID         uint64 `gorm:"primaryKey"`
	RunID      uint64 `gorm:"index"`
	HostID     uint64
	Task       string `gorm:"size:512"`
	Status     string
	Diff       string `gorm:"type:text"`
	Error      string
	FinishedAt time.Time
}
