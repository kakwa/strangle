package store

import (
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AnsibleRun is one host's result in one playbook execution. The aggregate
// run metadata stays in runs; org, collection and target are reached through
// the host. Tasks holds the ordered task results and Stats the recap counters.
// The UUIDv7 primary key is time-sortable, so rows can be paged in insertion
// order without a separate timestamp column.
type AnsibleRun struct {
	ID     uuid.UUID `gorm:"type:uuid;primaryKey"`
	RunID  uint64    `gorm:"not null;index;uniqueIndex:idx_ansible_run_host,priority:1"`
	HostID uint64    `gorm:"not null;index;uniqueIndex:idx_ansible_run_host,priority:2"`
	Host   Host      `gorm:"foreignKey:HostID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Tasks  []byte    `gorm:"type:jsonb"`
	Stats  []byte    `gorm:"type:jsonb"`
}

func (AnsibleRun) TableName() string { return "ansible_runs" }

// AnsibleTask is one entry of an AnsibleRun's Tasks jsonb array.
type AnsibleTask struct {
	Task   string `json:"task"`
	Status string `json:"status"`
	Diff   string `json:"diff,omitempty"`
	Error  string `json:"error,omitempty"`
}

// AnsibleStats is the jsonb stats blob of an AnsibleRun. Run-level metadata
// (mode, status, timing, success) lives on runs; only the recap is repeated
// per host.
type AnsibleStats struct {
	TaskCounters
}

// BeforeCreate assigns a time-sortable UUIDv7 when the caller did not provide
// one. google/uuid keeps a monotonic counter within the same millisecond.
func (a *AnsibleRun) BeforeCreate(*gorm.DB) error {
	if a.ID != uuid.Nil {
		return nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	a.ID = id
	return nil
}
