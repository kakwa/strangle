package model

// Group is a named set of users. Groups can belong to several orgs, and an
// org can contain several groups; the link is org_groups.
type Group struct {
	ID    uint64        `gorm:"primaryKey"`
	Name  string        `gorm:"size:255;not null;uniqueIndex"`
	Orgs  []*Org        `gorm:"many2many:org_groups;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Users []*UserRecord `gorm:"many2many:group_users;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

func (Group) TableName() string { return "groups" }
