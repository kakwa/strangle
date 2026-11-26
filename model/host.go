package model

import "time"

// Host is one appearance of a hostname in a specific context. The same
// hostname can be present several times when it is seen by different
// playbooks or inventories, so (collection_id, name, playbook, inventory) is
// the unique key. Facts is the latest ansible_facts blob collected for it.
// The collection name and org are reached through Collection, not copied here.
type Host struct {
	ID            uint64     `gorm:"primaryKey"`
	CollectionID  uint64     `gorm:"not null;index;uniqueIndex:idx_host_context,priority:1"`
	Collection    Collection `gorm:"foreignKey:CollectionID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Name          string     `gorm:"size:255;not null;uniqueIndex:idx_host_context,priority:2"`
	Playbook      string     `gorm:"size:512;not null;uniqueIndex:idx_host_context,priority:3"`
	Inventory     string     `gorm:"size:1024;not null;uniqueIndex:idx_host_context,priority:4"`
	IP            string
	User          string
	Groups        string `gorm:"size:2048"` // comma-separated group names
	InInventory   bool
	Active        bool
	FirstSeenAt   time.Time
	LastSeenAt    time.Time
	LastRunAt     time.Time
	LastStatus    string
	LastChangeAt  *time.Time
	DisappearedAt *time.Time
	Facts         []byte `gorm:"type:jsonb"`
}

func (Host) TableName() string { return "hosts" }
