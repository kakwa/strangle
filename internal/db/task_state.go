package db

import "time"

// TaskState is the latest known state of one task on one host.
type TaskState struct {
	ID            uint64 `gorm:"primaryKey"`
	HostID        uint64 `gorm:"uniqueIndex:idx_taskstate_host_task"`
	Task          string `gorm:"size:512;uniqueIndex:idx_taskstate_host_task"`
	LastRunID     uint64
	LastRunAt     time.Time
	LastStatus    string
	LastChangedAt *time.Time
	LastDiff      string `gorm:"type:text"`
	LastError     string
}
