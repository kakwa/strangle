package db

import (
	"encoding/json"
	"time"
)

// Org is a namespace that owns collections. Users and groups are linked to
// orgs through many-to-many join tables; every owned row also keeps a
// denormalized org_id so ownership can be checked without a join.
type Org struct {
	ID          uint64        `gorm:"primaryKey"`
	Name        string        `gorm:"size:255;not null;uniqueIndex"`
	Users       []*UserRecord `gorm:"many2many:org_users;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Groups      []*Group      `gorm:"many2many:org_groups;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Collections []Collection  `gorm:"foreignKey:OrgID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

func (Org) TableName() string { return "orgs" }

// Group is a named set of users. Groups can belong to several orgs, and an
// org can contain several groups; the link is org_groups.
type Group struct {
	ID    uint64        `gorm:"primaryKey"`
	Name  string        `gorm:"size:255;not null;uniqueIndex"`
	Orgs  []*Org        `gorm:"many2many:org_groups;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Users []*UserRecord `gorm:"many2many:group_users;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

func (Group) TableName() string { return "groups" }

// Collection is the database counterpart of one collection.yml entry. Its
// content is replaced from the YAML at startup, so the file stays the source
// of truth while the API and foreign keys can refer to stable numeric IDs.
// Options, Playbooks and Settings are jsonb columns: they mirror the nested
// YAML shapes and change whenever the project is reloaded.
type Collection struct {
	ID          uint64       `gorm:"primaryKey"`
	OrgID       uint64       `gorm:"not null;index;uniqueIndex:idx_collection_org_name,priority:1"`
	Org         Org          `gorm:"foreignKey:OrgID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Name        string       `gorm:"size:255;not null;uniqueIndex:idx_collection_org_name,priority:2"`
	Path        string       `gorm:"size:1024"`
	Options     []byte       `gorm:"type:jsonb"`
	Playbooks   []byte       `gorm:"type:jsonb"`
	Settings    []byte       `gorm:"type:jsonb"`
	Schedule    string       `gorm:"size:255"`
	Hosts       []Host       `gorm:"foreignKey:CollectionID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	AnsibleRuns []AnsibleRun `gorm:"foreignKey:CollectionID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

func (Collection) TableName() string { return "collections" }

// Host is one appearance of a hostname in a specific context. The same
// hostname can be present several times when it is seen by different
// playbooks or inventories, so (collection_id, name, playbook, inventory) is
// the unique key. Facts is the latest ansible_facts blob collected for it.
type Host struct {
	ID           uint64     `gorm:"primaryKey"`
	OrgID        uint64     `gorm:"not null;index"`
	Org          Org        `gorm:"foreignKey:OrgID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	CollectionID uint64     `gorm:"not null;index;uniqueIndex:idx_host_context,priority:1"`
	Collection   Collection `gorm:"foreignKey:CollectionID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	// Target is the denormalized collection name used by the existing API.
	Target        string `gorm:"size:255;index"`
	Name          string `gorm:"size:255;not null;uniqueIndex:idx_host_context,priority:2"`
	Playbook      string `gorm:"size:512;not null;uniqueIndex:idx_host_context,priority:3"`
	Inventory     string `gorm:"size:1024;not null;uniqueIndex:idx_host_context,priority:4"`
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
	Facts         []byte       `gorm:"type:jsonb"`
	AnsibleRuns   []AnsibleRun `gorm:"foreignKey:HostID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

func (Host) TableName() string { return "hosts" }

// AnsibleRun is one host's result in one playbook execution. Tasks is the
// ordered array of task results from the captured output; Stats holds the
// recap counters (and the run mode, duration and status) as jsonb. RunID links
// back to the aggregate run row so the existing run APIs keep working.
type AnsibleRun struct {
	ID           uint64     `gorm:"primaryKey;autoIncrement"`
	OrgID        uint64     `gorm:"not null;index"`
	Org          Org        `gorm:"foreignKey:OrgID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	CollectionID uint64     `gorm:"not null;index"`
	Collection   Collection `gorm:"foreignKey:CollectionID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	HostID       uint64     `gorm:"not null;index"`
	Host         Host       `gorm:"foreignKey:HostID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	RunID        uint64     `gorm:"index"`
	Mode         string     `gorm:"size:32;index"`
	Tasks        []byte     `gorm:"type:jsonb"`
	Stats        []byte     `gorm:"type:jsonb"`
	Status       string     `gorm:"size:32"`
	Success      bool
	StartedAt    time.Time
	FinishedAt   time.Time `gorm:"index"`
}

func (AnsibleRun) TableName() string { return "ansible_runs" }

// AnsibleTask is one entry of an AnsibleRun's Tasks jsonb array.
type AnsibleTask struct {
	Task   string `json:"task"`
	Status string `json:"status"`
	Diff   string `json:"diff,omitempty"`
	Error  string `json:"error,omitempty"`
}

// AnsibleStats is the jsonb stats blob of an AnsibleRun.
type AnsibleStats struct {
	Mode        string `json:"mode"`
	Status      string `json:"status"`
	Ok          int    `json:"ok"`
	Changed     int    `json:"changed"`
	Failed      int    `json:"failed"`
	Unreachable int    `json:"unreachable"`
	Skipped     int    `json:"skipped"`
	Rescued     int    `json:"rescued"`
	Ignored     int    `json:"ignored"`
	DurationMS  int64  `json:"duration_ms"`
	Success     bool   `json:"success"`
}

func jsonBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}
