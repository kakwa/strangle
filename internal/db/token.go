package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/auth"
	"gorm.io/gorm"
)

// TokenRecord is the api_tokens table: one row per API token. TokenHash is
// the SHA-256 of the raw value, which is never stored.
type TokenRecord struct {
	ID         uint64 `gorm:"primaryKey"`
	Owner      string `gorm:"size:255;uniqueIndex:idx_api_tokens_scope_name,priority:1"`
	Org        string `gorm:"size:255;uniqueIndex:idx_api_tokens_scope_name,priority:2"`
	Collection string `gorm:"size:255;uniqueIndex:idx_api_tokens_scope_name,priority:3"`
	Name       string `gorm:"size:255;uniqueIndex:idx_api_tokens_scope_name,priority:4"`
	TokenHash  string `gorm:"column:token_hash;size:64;uniqueIndex"`
	Prefix     string `gorm:"size:32"`
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
}

// TableName pins the table name; gorm's default for TokenRecord would be
// token_records.
func (TokenRecord) TableName() string { return "api_tokens" }

// TokenStore implements auth.TokenStore on the api_tokens table.
type TokenStore struct {
	db *gorm.DB
}

// Tokens returns the auth.TokenStore backed by this database.
func (d *DB) Tokens() *TokenStore {
	return &TokenStore{db: d.gdb}
}

func (t *TokenStore) Create(ctx context.Context, owner, org, collection, name string, ttl time.Duration) (string, *auth.Token, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, errors.New("db: token name is empty")
	}
	raw, err := auth.NewTokenValue()
	if err != nil {
		return "", nil, err
	}
	now := time.Now()
	var expires *time.Time
	if ttl > 0 {
		x := now.Add(ttl)
		expires = &x
	}
	rec := TokenRecord{
		Owner:      owner,
		Org:        org,
		Collection: collection,
		Name:       name,
		TokenHash:  auth.TokenKey(raw),
		Prefix:     raw[:len(auth.TokenPrefix)+8],
		CreatedAt:  now,
		ExpiresAt:  expires,
	}
	if err := t.db.WithContext(ctx).Create(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return "", nil, fmt.Errorf("db: token %q already exists", name)
		}
		return "", nil, err
	}
	return raw, toToken(&rec), nil
}

func (t *TokenStore) Lookup(ctx context.Context, raw string) (*auth.Token, error) {
	var rec TokenRecord
	if err := t.db.WithContext(ctx).
		Where("token_hash = ?", auth.TokenKey(raw)).
		First(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, auth.ErrNotFound
		}
		return nil, err
	}
	now := time.Now()
	if rec.ExpiresAt != nil && now.After(*rec.ExpiresAt) {
		return nil, auth.ErrNotFound
	}
	if err := t.db.WithContext(ctx).Model(&rec).Update("last_used_at", now).Error; err != nil {
		return nil, err
	}
	rec.LastUsedAt = &now
	return toToken(&rec), nil
}

func (t *TokenStore) List(ctx context.Context, owner, org, collection string) ([]*auth.Token, error) {
	var recs []TokenRecord
	q := t.db.WithContext(ctx)
	if owner != "" {
		q = q.Where("owner = ?", owner)
	}
	if org != "" {
		q = q.Where("org = ?", org)
	}
	if collection != "" {
		q = q.Where("collection = ?", collection)
	}
	if err := q.Order("org").Order("collection").Order("name").Find(&recs).Error; err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]*auth.Token, 0, len(recs))
	for i := range recs {
		if recs[i].ExpiresAt != nil && now.After(*recs[i].ExpiresAt) {
			continue
		}
		out = append(out, toToken(&recs[i]))
	}
	return out, nil
}

func (t *TokenStore) Remove(ctx context.Context, owner, org, collection, name string) error {
	res := t.db.WithContext(ctx).
		Where("owner = ? AND org = ? AND collection = ? AND name = ?", owner, org, collection, name).
		Delete(&TokenRecord{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return auth.ErrNotFound
	}
	return nil
}

func (t *TokenStore) Close() error { return nil }

func toToken(r *TokenRecord) *auth.Token {
	return &auth.Token{
		Owner:      r.Owner,
		Org:        r.Org,
		Collection: r.Collection,
		Name:       r.Name,
		Prefix:     r.Prefix,
		CreatedAt:  r.CreatedAt,
		Expires:    r.ExpiresAt,
		LastUsedAt: r.LastUsedAt,
	}
}
