package db

import (
	"errors"
	"fmt"

	"github.com/kakwa/strangle/internal/cfg"
	"gorm.io/gorm"
)

// Org is a namespace that owns collections. Users and groups are linked to
// orgs through many-to-many join tables.
type Org struct {
	ID          uint64        `gorm:"primaryKey"`
	Name        string        `gorm:"size:255;not null;uniqueIndex"`
	Users       []*UserRecord `gorm:"many2many:org_users;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Groups      []*Group      `gorm:"many2many:org_groups;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Collections []Collection  `gorm:"foreignKey:OrgID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

func (Org) TableName() string { return "orgs" }

func defaultOrgID(tx *gorm.DB) uint64 {
	org, err := firstOrCreateOrg(tx, cfg.DefaultOrg)
	if err != nil {
		return 0
	}
	return org.ID
}

func firstOrCreateOrg(tx *gorm.DB, name string) (Org, error) {
	var org Org
	err := tx.Where("name = ?", name).First(&org).Error
	if err == nil {
		return org, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Org{}, err
	}
	org = Org{Name: name}
	if err := tx.Create(&org).Error; err != nil {
		return Org{}, fmt.Errorf("insert org %q: %w", name, err)
	}
	return org, nil
}
