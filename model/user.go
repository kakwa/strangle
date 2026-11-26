package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kakwa/strangle/security/auth"
	"gorm.io/gorm"
)

// UserRecord is the users table: one row per local UI account. Org and group
// membership are expressed solely by the many-to-many join tables.
type UserRecord struct {
	ID     uint64   `gorm:"primaryKey"`
	Name   string   `gorm:"size:255;uniqueIndex"`
	Hash   string   `gorm:"size:512"` // argon2id PHC string
	Orgs   []*Org   `gorm:"many2many:org_users;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Groups []*Group `gorm:"many2many:group_users;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

// TableName pins the table name; gorm's default for UserRecord would be
// user_records.
func (UserRecord) TableName() string { return "users" }

// UserStore implements auth.UserStore on the users table.
type UserStore struct {
	db *gorm.DB
}

// Users returns the auth.UserStore backed by this database.
func (d *DB) Users() *UserStore {
	return &UserStore{db: d.gdb}
}

func (u *UserStore) CreateUser(ctx context.Context, name, hash string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("db: user name is empty")
	}
	if err := u.db.WithContext(ctx).Create(&UserRecord{Name: name, Hash: hash}).Error; err != nil {
		// The postgres driver translates unique-constraint violations
		// (SQLSTATE 23505) into gorm.ErrDuplicatedKey.
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return fmt.Errorf("db: user %q already exists", name)
		}
		return err
	}
	return nil
}

func (u *UserStore) GetUser(ctx context.Context, name string) (*auth.User, error) {
	var rec UserRecord
	if err := u.db.WithContext(ctx).Where("name = ?", name).First(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, auth.ErrNotFound
		}
		return nil, err
	}
	return &auth.User{Name: rec.Name, Hash: rec.Hash}, nil
}

func (u *UserStore) ListUsers(ctx context.Context) ([]*auth.User, error) {
	var recs []UserRecord
	if err := u.db.WithContext(ctx).Order("name").Find(&recs).Error; err != nil {
		return nil, err
	}
	out := make([]*auth.User, 0, len(recs))
	for _, r := range recs {
		out = append(out, &auth.User{Name: r.Name, Hash: r.Hash})
	}
	return out, nil
}

func (u *UserStore) Count(ctx context.Context) (int, error) {
	var n int64
	if err := u.db.WithContext(ctx).Model(&UserRecord{}).Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}

func (u *UserStore) RemoveUser(ctx context.Context, name string) error {
	res := u.db.WithContext(ctx).Where("name = ?", name).Delete(&UserRecord{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return auth.ErrNotFound
	}
	return nil
}

func (u *UserStore) Close() error { return nil }
