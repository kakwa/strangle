package db

import (
	"context"
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

// OrgRecord is the API representation of a stored organization. A stored org
// may have no configured collections yet: organizations are created through
// POST /api/v1/orgs and collections are added later through collection.yml.
type OrgRecord struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

// CreateOrg inserts an organization. It returns an error when the name is
// already present, so callers can map that to HTTP 409.
func (d *DB) CreateOrg(ctx context.Context, name string) (OrgRecord, error) {
	org := Org{Name: name}
	if err := d.gdb.WithContext(ctx).Create(&org).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return OrgRecord{}, fmt.Errorf("org %q already exists: %w", name, err)
		}
		return OrgRecord{}, err
	}
	return OrgRecord{ID: org.ID, Name: org.Name}, nil
}

// ListOrgs returns all stored organizations in name order, including the
// default org inserted at startup and orgs created through the API.
func (d *DB) ListOrgs(ctx context.Context) ([]OrgRecord, error) {
	var rows []Org
	if err := d.gdb.WithContext(ctx).Order("name").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]OrgRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, OrgRecord{ID: row.ID, Name: row.Name})
	}
	return out, nil
}

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
