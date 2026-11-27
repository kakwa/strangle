package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/kakwa/strangle/cfg"
	"gorm.io/gorm"
)

// Collection is the database counterpart of one collection.yml entry. Its
// content is replaced from the YAML at startup, so the file stays the source
// of truth while the API and foreign keys can refer to stable numeric IDs.
// Options, Playbooks and Settings are jsonb columns mirroring the YAML.
type Collection struct {
	ID        uint64 `gorm:"primaryKey"`
	OrgID     uint64 `gorm:"not null;index;uniqueIndex:idx_collection_org_name,priority:1"`
	Org       Org    `gorm:"foreignKey:OrgID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
	Name      string `gorm:"size:255;not null;uniqueIndex:idx_collection_org_name,priority:2"`
	Path      string `gorm:"size:1024"`
	Options   []byte `gorm:"type:jsonb"`
	Playbooks []byte `gorm:"type:jsonb"`
	Settings  []byte `gorm:"type:jsonb"`
	Schedule  string `gorm:"size:255"`
	Hosts     []Host `gorm:"foreignKey:CollectionID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE;"`
}

func (Collection) TableName() string { return "collections" }

// SyncCollections replaces the collections table with the contents of the
// loaded collection.yml. Called at startup (after any git checkout has been
// refreshed), so the file remains the source of truth while the database gets
// stable IDs and foreign keys for hosts and runs.
func (d *DB) SyncCollections(ctx context.Context, targets []cfg.TargetConfig) error {
	return d.gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		orgIDs := map[string]uint64{cfg.DefaultOrg: defaultOrgID(tx)}
		if orgIDs[cfg.DefaultOrg] == 0 {
			return errors.New("default org is missing")
		}

		for _, t := range targets {
			name := t.Org
			if name == "" {
				name = cfg.DefaultOrg
			}
			if _, ok := orgIDs[name]; ok {
				continue
			}
			org, err := firstOrCreateOrg(tx, name)
			if err != nil {
				return err
			}
			orgIDs[name] = org.ID
		}

		kept := make(map[string]struct{}, len(targets))
		for _, t := range targets {
			orgName := t.Org
			if orgName == "" {
				orgName = cfg.DefaultOrg
			}
			orgID := orgIDs[orgName]

			options := make(map[string]cfg.PlaybookOptions, len(t.Playbooks))
			for _, pb := range t.Playbooks {
				options[pb.Name] = pb.Options
			}
			settings := struct {
				Path        string                `json:"path"`
				Git         cfg.GitConfig         `json:"git,omitempty"`
				Retention   cfg.RetentionConfig   `json:"retention,omitempty"`
				HostCleanup cfg.HostCleanupConfig `json:"host_cleanup,omitempty"`
			}{
				Path: t.Path, Git: t.Git, Retention: t.Retention, HostCleanup: t.HostCleanup,
			}

			playbooks := t.Playbooks
			if playbooks == nil {
				playbooks = []cfg.PlaybookConfig{}
			}
			optionsB, playbooksB, settingsB := jsonBytes(options), jsonBytes(playbooks), jsonBytes(settings)
			var coll Collection
			err := tx.Where("org_id = ? AND name = ?", orgID, t.Name).First(&coll).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				coll = Collection{OrgID: orgID, Name: t.Name, Path: t.Path, Options: optionsB,
					Playbooks: playbooksB, Settings: settingsB, Schedule: t.Schedule,
				}
				if err := tx.Create(&coll).Error; err != nil {
					return fmt.Errorf("insert collection %q: %w", t.Name, err)
				}
			case err != nil:
				return err
			default:
				// Overwrite every configured field, including empty ones: the
				// YAML is authoritative for the whole row.
				if err := tx.Model(&coll).Updates(map[string]any{
					"path": t.Path, "options": optionsB, "playbooks": playbooksB,
					"settings": settingsB, "schedule": t.Schedule,
				}).Error; err != nil {
					return fmt.Errorf("update collection %q: %w", t.Name, err)
				}
			}
			kept[fmt.Sprintf("%d/%s", orgID, t.Name)] = struct{}{}
		}

		// Collections removed from the file are dropped with their hosts and
		// history, so a reload cannot leave stale data behind.
		var existing []Collection
		if err := tx.Find(&existing).Error; err != nil {
			return err
		}
		for _, coll := range existing {
			if _, ok := kept[fmt.Sprintf("%d/%s", coll.OrgID, coll.Name)]; ok {
				continue
			}
			if err := deleteCollection(tx, coll.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func deleteCollection(tx *gorm.DB, id uint64) error {
	var hosts []Host
	if err := tx.Where("collection_id = ?", id).Find(&hosts).Error; err != nil {
		return err
	}
	ids := make([]uint64, 0, len(hosts))
	for _, h := range hosts {
		ids = append(ids, h.ID)
	}
	if len(ids) > 0 {
		for _, tbl := range []any{&HostRun{}, &TaskState{}, &TaskRunResult{}, &AnsibleRun{}} {
			if err := tx.Where("host_id IN ?", ids).Delete(tbl).Error; err != nil {
				return err
			}
		}
	}
	if err := tx.Delete(&Host{}, "collection_id = ?", id).Error; err != nil {
		return err
	}
	return tx.Delete(&Collection{}, "id = ?", id).Error
}

// ensureCollection returns the collection ID for a run's target, creating a
// placeholder collection when a run arrives before the startup sync (as in
// tests and for ad-hoc targets).
func ensureCollection(tx *gorm.DB, name string) (uint64, error) {
	var coll Collection
	err := tx.Where("name = ?", name).Order("org_id").First(&coll).Error
	if err == nil {
		return coll.ID, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}
	orgID := defaultOrgID(tx)
	if orgID == 0 {
		return 0, errors.New("default org is missing")
	}
	coll = Collection{OrgID: orgID, Name: name, Options: []byte("{}"), Playbooks: []byte("[]"), Settings: []byte("{}")}
	if err := tx.Create(&coll).Error; err != nil {
		return 0, fmt.Errorf("insert collection %q: %w", name, err)
	}
	return coll.ID, nil
}
