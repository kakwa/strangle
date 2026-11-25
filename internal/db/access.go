package db

import (
	"context"
	"errors"

	"github.com/kakwa/strangle/internal/auth"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CollectionMemberRecord is one user-to-collection grant. Org and collection
// names are the same identifiers exposed by the versioned API.
type CollectionMemberRecord struct {
	ID         uint64 `gorm:"primaryKey"`
	Username   string `gorm:"size:255;uniqueIndex:idx_collection_member"`
	Org        string `gorm:"size:255;uniqueIndex:idx_collection_member"`
	Collection string `gorm:"size:255;uniqueIndex:idx_collection_member"`
}

func (CollectionMemberRecord) TableName() string { return "collection_members" }

// CollectionAccess implements auth.CollectionAccess on the
// collection_members table.
type CollectionAccess struct {
	db *gorm.DB
}

// Access returns the collection membership store backed by this database.
func (d *DB) Access() *CollectionAccess {
	return &CollectionAccess{db: d.db}
}

func (a *CollectionAccess) Add(ctx context.Context, username, org, collection string) error {
	if username == "" || org == "" || collection == "" {
		return auth.ErrInvalidMembership
	}
	rec := CollectionMemberRecord{Username: username, Org: org, Collection: collection}
	return a.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&rec).Error
}

func (a *CollectionAccess) Remove(ctx context.Context, username, org, collection string) error {
	return a.db.WithContext(ctx).
		Where("username = ? AND org = ? AND collection = ?", username, org, collection).
		Delete(&CollectionMemberRecord{}).Error
}

func (a *CollectionAccess) List(ctx context.Context, username string) ([]auth.CollectionMembership, error) {
	q := a.db.WithContext(ctx).Model(&CollectionMemberRecord{}).Order("username, org, collection")
	if username != "" {
		q = q.Where("username = ?", username)
	}
	var recs []CollectionMemberRecord
	if err := q.Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]auth.CollectionMembership, 0, len(recs))
	for _, r := range recs {
		out = append(out, auth.CollectionMembership{
			Username: r.Username, Org: r.Org, Collection: r.Collection,
		})
	}
	return out, nil
}

func (a *CollectionAccess) HasAny(ctx context.Context) (bool, error) {
	var n int64
	if err := a.db.WithContext(ctx).Model(&CollectionMemberRecord{}).Count(&n).Error; err != nil {
		return false, err
	}
	return n > 0, nil
}

func (a *CollectionAccess) Allows(ctx context.Context, username, org, collection string) (bool, error) {
	var n int64
	err := a.db.WithContext(ctx).Model(&CollectionMemberRecord{}).
		Where("username = ? AND org = ? AND collection = ?", username, org, collection).
		Count(&n).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}
	return n > 0, nil
}

func (a *CollectionAccess) Close() error { return nil }
