// Package db implements the store.Store interface on PostgreSQL via gorm.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kakwa/strangle/internal/cfg"
	"github.com/kakwa/strangle/internal/store"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB is a store.Store backed by PostgreSQL.
type DB struct {
	db *gorm.DB
}

// Open connects to the PostgreSQL database described by dsn and migrates the
// schema.
func Open(ctx context.Context, dsn string) (*DB, error) {
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
		// Turn SQLSTATE 23505 into gorm.ErrDuplicatedKey so stores can map
		// duplicate users/tokens to their domain errors.
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("acquire sql.DB: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	// Associations are migrated too: orgs/groups/users (and their join
	// tables), collections, context-scoped hosts and per-host ansible runs.
	if err := gdb.WithContext(ctx).AutoMigrate(
		&Org{}, &Group{}, &UserRecord{}, &Collection{}, &Host{}, &AnsibleRun{},
		&Run{}, &HostRun{}, &TaskState{}, &TaskRunResult{}, &TokenRecord{}, &CollectionMemberRecord{},
	); err != nil {
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	// AutoMigrate never drops indexes. Replace the old per-target host index
	// with the context key (collection, hostname, playbook, inventory).
	for _, idx := range []string{"idx_hosts_name", "idx_host_target_name"} {
		if err := gdb.WithContext(ctx).Exec("DROP INDEX IF EXISTS " + idx).Error; err != nil {
			return nil, fmt.Errorf("drop legacy hosts index: %w", err)
		}
	}

	// Every installation has the default org, even before the collection
	// file is loaded. Collections without an explicit org belong to it.
	if err := gdb.WithContext(ctx).Where("name = ?", cfg.DefaultOrg).
		FirstOrCreate(&Org{}, Org{Name: cfg.DefaultOrg}).Error; err != nil {
		return nil, fmt.Errorf("initialize default org: %w", err)
	}
	return &DB{db: gdb}, nil
}

// runIDCounter allocates run IDs before the database assigns them. Runs are
// created before their queue job is inserted, so the ID must come from here.
var runIDCounter atomic.Uint64

func init() {
	runIDCounter.Store(uint64(time.Now().UnixNano()))
}

func (d *DB) CreateRun(ctx context.Context, in *store.RunRecord) error {
	run := toRunModel(*in)
	if run.ID == 0 {
		run.ID = runIDCounter.Add(1)
		in.ID = run.ID
	}
	if run.StartedAt.IsZero() {
		run.StartedAt = time.Now().UTC()
		in.StartedAt = run.StartedAt
	}
	if run.State == "" {
		run.State = store.RunStateQueued
		in.State = run.State
	}
	if err := d.db.WithContext(ctx).Create(&run).Error; err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	*in = fromRunModel(run)
	return nil
}

func (d *DB) UpdateRun(ctx context.Context, in store.RunRecord) error {
	run := toRunModel(in)
	res := d.db.WithContext(ctx).Model(&Run{}).Where("id = ?", in.ID).Updates(map[string]any{
		"target": run.Target, "playbook": run.Playbook, "started_at": run.StartedAt,
		"finished_at": run.FinishedAt, "duration_ms": run.DurationMS, "exit_code": run.ExitCode,
		"success": run.Success, "hosts": run.Hosts, "changed_hosts": run.ChangedHosts,
		"failed_hosts": run.FailedHosts, "output": run.Output, "state": run.State,
		"canceled": run.Canceled, "check": run.Check, "job_id": run.JobID, "inventory": run.Inventory,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (d *DB) SaveRun(ctx context.Context, in store.RunInput) error {
	// A run normally gets its ID from CreateRun before its queue job is
	// inserted. Allocate one here too so inserting a completed run directly
	// cannot collide with IDs assigned earlier in the same database.
	if in.Run.ID == 0 {
		in.Run.ID = runIDCounter.Add(1)
	}
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		run := toRunModel(in.Run)
		if in.Run.ID != 0 {
			var existing Run
			err := tx.Where("id = ?", in.Run.ID).First(&existing).Error
			if err == nil {
				run.ID = existing.ID
				// The job ID is written by the enqueuer after the job is
				// inserted and can race with a fast worker; never clear it
				// when finalizing a run.
				if run.JobID == "" {
					run.JobID = existing.JobID
				}
				if existing.Canceled {
					run.State = store.RunStateCancelled
					run.Canceled = true
				}
				// Preserve the ID allocated at creation time; gorm's Create with
				// an existing primary key would otherwise conflict.
				if err := tx.Model(&Run{}).Where("id = ?", run.ID).Updates(map[string]any{
					"target": run.Target, "playbook": run.Playbook, "started_at": run.StartedAt,
					"finished_at": run.FinishedAt, "duration_ms": run.DurationMS, "exit_code": run.ExitCode,
					"success": run.Success, "hosts": run.Hosts, "changed_hosts": run.ChangedHosts,
					"failed_hosts": run.FailedHosts, "output": run.Output, "state": run.State,
					"canceled": run.Canceled, "job_id": run.JobID, "inventory": run.Inventory,
				}).Error; err != nil {
					return fmt.Errorf("update run: %w", err)
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			} else if err := tx.Create(&run).Error; err != nil {
				return fmt.Errorf("insert run: %w", err)
			}
		} else if err := tx.Create(&run).Error; err != nil {
			return fmt.Errorf("insert run: %w", err)
		}

		// Collect every host name involved so IDs can be resolved once.
		names := map[string]struct{}{}
		for _, ih := range in.Inventory {
			names[ih.Name] = struct{}{}
		}
		for _, h := range in.Hosts {
			names[h.Host] = struct{}{}
		}
		for _, n := range in.ScopeNames {
			names[n] = struct{}{}
		}
		for _, t := range in.Tasks {
			names[t.Host] = struct{}{}
		}
		nameList := make([]string, 0, len(names))
		for n := range names {
			nameList = append(nameList, n)
		}

		collectionID, err := ensureCollection(tx, in.Run.Target)
		if err != nil {
			return err
		}

		// A host is identified by collection + hostname + playbook + inventory:
		// the same hostname seen in another context is a different row.
		hostIDs := map[string]uint64{}
		if len(nameList) > 0 {
			var existing []Host
			if err := tx.Where("collection_id = ? AND playbook = ? AND inventory = ? AND name IN ?",
				collectionID, in.Run.Playbook, in.Run.Inventory, nameList).Find(&existing).Error; err != nil {
				return err
			}
			for _, h := range existing {
				hostIDs[h.Name] = h.ID
			}
			for _, n := range nameList {
				if _, ok := hostIDs[n]; ok {
					continue
				}
				h := Host{
					CollectionID: collectionID,
					Name:         n, Playbook: in.Run.Playbook, Inventory: in.Run.Inventory,
					FirstSeenAt: run.FinishedAt,
				}
				if err := tx.Create(&h).Error; err != nil {
					return fmt.Errorf("insert host %q: %w", n, err)
				}
				hostIDs[n] = h.ID
			}
		}

		// Inventory snapshot: update known fields and membership in this
		// context only.
		for _, ih := range in.Inventory {
			updates := map[string]any{
				"ip":           ih.IP,
				"user":         ih.User,
				"groups":       strings.Join(ih.Groups, ","),
				"in_inventory": true,
			}
			if err := tx.Model(&Host{}).Where("id = ?", hostIDs[ih.Name]).Updates(updates).Error; err != nil {
				return err
			}
		}
		if len(in.Inventory) > 0 {
			invHostIDs := make([]uint64, 0, len(in.Inventory))
			for _, ih := range in.Inventory {
				invHostIDs = append(invHostIDs, hostIDs[ih.Name])
			}
			if err := tx.Model(&Host{}).
				Where("collection_id = ? AND playbook = ? AND inventory = ? AND id NOT IN ?",
					collectionID, in.Run.Playbook, in.Run.Inventory, invHostIDs).
				UpdateColumn("in_inventory", false).Error; err != nil {
				return err
			}
		}

		// Recap lines.
		inRecap := map[string]struct{}{}
		for _, h := range in.Hosts {
			inRecap[h.Host] = struct{}{}
			status := store.HostStatus(h.Ok, h.Changed, h.Failed, h.Unreachable)
			updates := map[string]any{
				"last_seen_at":   run.FinishedAt,
				"last_run_at":    run.FinishedAt,
				"last_status":    status,
				"active":         true,
				"disappeared_at": nil,
			}
			if facts := in.Facts[h.Host]; len(facts) > 0 {
				updates["facts"] = []byte(facts)
			}
			if err := tx.Model(&Host{}).Where("id = ?", hostIDs[h.Host]).Updates(updates).Error; err != nil {
				return err
			}
			hr := toHostRunModel(run, hostIDs[h.Host], h)
			if err := tx.Create(&hr).Error; err != nil {
				return fmt.Errorf("insert host run: %w", err)
			}
		}

		// Per-host run history: tasks and recap counters as jsonb. Everything
		// else (org, collection, target, mode, timing) is reached via the host
		// and the aggregate run row, so it is not stored twice here.
		tasksByHost := map[string][]AnsibleTask{}
		for _, t := range in.Tasks {
			tasksByHost[t.Host] = append(tasksByHost[t.Host], AnsibleTask{
				Task: t.Task, Status: t.Status, Diff: t.LastDiff, Error: t.LastError,
			})
		}
		for _, h := range in.Hosts {
			ar := AnsibleRun{
				RunID:  run.ID,
				HostID: hostIDs[h.Host],
				Tasks:  jsonBytes(tasksByHost[h.Host]),
				Stats: jsonBytes(AnsibleStats{TaskCounters: TaskCounters{
					Ok: h.Ok, Changed: h.Changed, Failed: h.Failed,
					Unreachable: h.Unreachable, Skipped: h.Skipped,
					Rescued: h.Rescued, Ignored: h.Ignored,
				}}),
			}
			if err := tx.Create(&ar).Error; err != nil {
				return fmt.Errorf("insert ansible run: %w", err)
			}
		}

		// Scope hosts that did not answer are disappeared.
		for _, name := range in.ScopeNames {
			if _, ok := inRecap[name]; ok {
				continue
			}
			if err := tx.Model(&Host{}).Where("id = ?", hostIDs[name]).
				Updates(map[string]any{
					"active":         false,
					"disappeared_at": gorm.Expr("COALESCE(disappeared_at, ?)", run.FinishedAt),
				}).Error; err != nil {
				return err
			}
		}

		// Task states (latest view) and append-only task history.
		changedHosts := map[uint64]struct{}{}
		taskRuns := make([]TaskRunResult, 0, len(in.Tasks))
		for _, t := range in.Tasks {
			hostID := hostIDs[t.Host]
			taskRuns = append(taskRuns, TaskRunResult{
				RunID:      run.ID,
				HostID:     hostID,
				Task:       t.Task,
				Status:     t.Status,
				Diff:       t.LastDiff,
				Error:      t.LastError,
				FinishedAt: run.FinishedAt,
			})
			var ts TaskState
			err := tx.Where("host_id = ? AND task = ?", hostID, t.Task).First(&ts).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				ts = TaskState{
					HostID:     hostID,
					Task:       t.Task,
					LastRunID:  run.ID,
					LastRunAt:  run.FinishedAt,
					LastStatus: t.Status,
				}
				if t.Status == "changed" {
					ts.LastChangedAt = &run.FinishedAt
					ts.LastDiff = t.LastDiff
				}
				if t.Status == "failed" {
					ts.LastError = t.LastError
				}
				if err := tx.Create(&ts).Error; err != nil {
					return fmt.Errorf("insert task state: %w", err)
				}
				if t.Status == "changed" {
					changedHosts[hostID] = struct{}{}
				}
				continue
			}
			if err != nil {
				return err
			}

			updates := map[string]any{
				"last_run_id": run.ID,
				"last_run_at": run.FinishedAt,
				"last_status": t.Status,
			}
			if t.Status == "changed" {
				updates["last_changed_at"] = run.FinishedAt
				updates["last_diff"] = t.LastDiff
				changedHosts[hostID] = struct{}{}
			}
			if t.Status == "failed" {
				updates["last_error"] = t.LastError
			}
			if err := tx.Model(&TaskState{}).Where("id = ?", ts.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		if len(taskRuns) > 0 {
			if err := tx.Create(&taskRuns).Error; err != nil {
				return fmt.Errorf("insert task history: %w", err)
			}
		}

		// Bump the host's last-change timestamp.
		for hostID := range changedHosts {
			if err := tx.Model(&Host{}).Where("id = ?", hostID).
				Update("last_change_at", run.FinishedAt).Error; err != nil {
				return err
			}
		}

		return nil
	})
}

func (d *DB) ListRuns(ctx context.Context, limit int) ([]store.RunRecord, error) {
	var runs []Run
	q := d.db.WithContext(ctx).Order("id DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&runs).Error; err != nil {
		return nil, err
	}
	out := make([]store.RunRecord, 0, len(runs))
	for _, r := range runs {
		out = append(out, fromRunModel(r))
	}
	return out, nil
}

func (d *DB) Run(ctx context.Context, id uint64) (store.RunRecord, error) {
	var run Run
	if err := d.db.WithContext(ctx).First(&run, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return store.RunRecord{}, fmt.Errorf("run %d: %w", id, store.ErrNotFound)
		}
		return store.RunRecord{}, err
	}
	return fromRunModel(run), nil
}

func (d *DB) ListHosts(ctx context.Context) ([]store.HostRecord, error) {
	// The collection name is the target exposed by the APIs; it is not stored
	// on hosts anymore, so the host view joins collections to get it.
	type hostRow struct {
		Host
		Target string `gorm:"column:target"`
	}
	var hosts []hostRow
	// The same hostname can have one row per playbook/inventory context; the
	// host view only shows its latest appearance per collection.
	if err := d.db.WithContext(ctx).
		Raw(`SELECT DISTINCT ON (c.name, h.name) h.*, c.name AS target
			FROM hosts h JOIN collections c ON c.id = h.collection_id
			ORDER BY c.name, h.name, h.last_run_at DESC, h.id DESC`).
		Scan(&hosts).Error; err != nil {
		return nil, err
	}
	out := make([]store.HostRecord, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, fromHostModel(h.Host, h.Target))
	}
	return out, nil
}

func (d *DB) HostRuns(ctx context.Context, host string, limit int) ([]store.HostRunRecord, error) {
	type row struct {
		HostRun
		HostName   string    `gorm:"column:hostname"`
		HostTarget string    `gorm:"column:hosttarget"`
		Playbook   string    `gorm:"column:playbook"`
		StartedAt  time.Time `gorm:"column:started_at"`
		DurationMS int64     `gorm:"column:duration_ms"`
		ExitCode   int       `gorm:"column:exit_code"`
		Success    bool      `gorm:"column:success"`
	}
	var rows []row
	q := d.db.WithContext(ctx).Model(&HostRun{}).
		Joins("JOIN hosts h ON h.id = host_runs.host_id").
		Joins("JOIN collections c ON c.id = h.collection_id").
		Joins("JOIN runs r ON r.id = host_runs.run_id").
		Select("DISTINCT ON (r.id) host_runs.*, h.name AS hostname, c.name AS hosttarget, r.playbook, r.started_at, r.duration_ms, r.exit_code, r.success").
		Where("h.name = ?", host).
		Order("r.id DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]store.HostRunRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.HostRunRecord{
			RunID:       r.RunID,
			Target:      r.HostTarget,
			Host:        r.HostName,
			Playbook:    r.Playbook,
			StartedAt:   r.StartedAt,
			DurationMS:  r.DurationMS,
			ExitCode:    r.ExitCode,
			Success:     r.Success,
			Status:      r.Status,
			Ok:          r.Ok,
			Changed:     r.Changed,
			Failed:      r.Failed,
			Unreachable: r.Unreachable,
			Skipped:     r.Skipped,
			Rescued:     r.Rescued,
			Ignored:     r.Ignored,
		})
	}
	return out, nil
}

func (d *DB) HostChanges(ctx context.Context, host string, limit int) ([]store.TaskStateRecord, error) {
	type row struct {
		TaskState
		HostName   string `gorm:"column:hostname"`
		HostTarget string `gorm:"column:hosttarget"`
	}
	var rows []row
	if err := d.db.WithContext(ctx).Model(&TaskState{}).
		Joins("JOIN hosts h ON h.id = task_states.host_id").
		Joins("JOIN collections c ON c.id = h.collection_id").
		Select("task_states.*, h.name AS hostname, c.name AS hosttarget").
		Where("h.name = ?", host).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	// Keep the newest state per (target, hostname, task) when a hostname has
	// rows in several contexts, then restore the recency ordering.
	latest := map[string]row{}
	for _, r := range rows {
		key := r.HostTarget + "\x00" + r.HostName + "\x00" + r.Task
		prev, ok := latest[key]
		if !ok || r.LastRunAt.After(prev.LastRunAt) ||
			(r.LastRunAt.Equal(prev.LastRunAt) && r.ID > prev.ID) {
			latest[key] = r
		}
	}
	rows = rows[:0]
	for _, r := range latest {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a := rows[i].LastChangedAt
		if a == nil {
			a = &rows[i].LastRunAt
		}
		b := rows[j].LastChangedAt
		if b == nil {
			b = &rows[j].LastRunAt
		}
		return a.After(*b)
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]store.TaskStateRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.TaskStateRecord{
			Target:        r.HostTarget,
			Host:          r.HostName,
			Task:          r.Task,
			Status:        r.LastStatus,
			LastRunID:     r.LastRunID,
			LastRunAt:     r.LastRunAt,
			LastChangedAt: r.LastChangedAt,
			LastDiff:      r.LastDiff,
			LastError:     r.LastError,
		})
	}
	return out, nil
}

func (d *DB) RunTasks(ctx context.Context, runID uint64) ([]store.TaskRunRecord, error) {
	var run Run
	if err := d.db.WithContext(ctx).Select("id").First(&run, runID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("run %d: %w", runID, store.ErrNotFound)
		}
		return nil, err
	}

	type row struct {
		TaskRunResult
		HostName   string `gorm:"column:hostname"`
		HostTarget string `gorm:"column:hosttarget"`
	}
	var rows []row
	if err := d.db.WithContext(ctx).Model(&TaskRunResult{}).
		Joins("JOIN hosts h ON h.id = task_run_results.host_id").
		Joins("JOIN collections c ON c.id = h.collection_id").
		Select("task_run_results.*, h.name AS hostname, c.name AS hosttarget").
		Where("task_run_results.run_id = ?", runID).
		Order("task_run_results.id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]store.TaskRunRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, store.TaskRunRecord{
			ID:         r.ID,
			RunID:      r.RunID,
			Target:     r.HostTarget,
			Host:       r.HostName,
			Task:       r.Task,
			Status:     r.Status,
			Diff:       r.Diff,
			Error:      r.Error,
			FinishedAt: r.FinishedAt,
		})
	}
	return out, nil
}

// PruneRuns removes finished runs of one collection that exceed the policy.
// Queued and running runs are kept so an in-flight River job cannot recreate
// a purged record when it finishes. Child rows are removed in the same
// transaction: the schema declares no cascading foreign keys.
func (d *DB) PruneRuns(ctx context.Context, target string, policy store.RetentionPolicy) (store.PruneResult, error) {
	var out store.PruneResult
	if !policy.Enabled() {
		return out, nil
	}

	type candidate struct {
		ID         uint64
		StartedAt  time.Time
		FinishedAt time.Time
	}
	var rows []candidate
	if err := d.db.WithContext(ctx).Model(&Run{}).
		Select("id", "started_at", "finished_at").
		Where("target = ?", target).
		Where("state NOT IN ?", []string{store.RunStateQueued, store.RunStateRunning}).
		Order("id DESC").
		Scan(&rows).Error; err != nil {
		return out, fmt.Errorf("list runs for retention: %w", err)
	}

	cutoff := time.Now().UTC().Add(-policy.MaxAge)
	ids := make([]uint64, 0, len(rows))
	keptCount := 0
	for _, r := range rows {
		remove := false
		if policy.MaxAge > 0 {
			timestamp := r.FinishedAt
			if timestamp.IsZero() {
				timestamp = r.StartedAt
			}
			remove = !timestamp.IsZero() && timestamp.Before(cutoff)
		}
		if !remove && policy.MaxCount > 0 && keptCount >= policy.MaxCount {
			remove = true
		}
		if remove {
			ids = append(ids, r.ID)
		} else {
			keptCount++
		}
	}
	if len(ids) == 0 {
		return out, nil
	}

	tx := d.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return out, fmt.Errorf("begin retention transaction: %w", tx.Error)
	}
	// Latest-state rows are kept, but must not link to a deleted run.
	if err := tx.Model(&TaskState{}).Where("last_run_id IN ?", ids).
		Update("last_run_id", 0).Error; err != nil {
		tx.Rollback()
		return out, fmt.Errorf("clear task state run links: %w", err)
	}
	if res := tx.Where("run_id IN ?", ids).Delete(&HostRun{}); res.Error != nil {
		tx.Rollback()
		return out, fmt.Errorf("delete host run history: %w", res.Error)
	} else {
		out.HostRuns = res.RowsAffected
	}
	if res := tx.Where("run_id IN ?", ids).Delete(&TaskRunResult{}); res.Error != nil {
		tx.Rollback()
		return out, fmt.Errorf("delete task results: %w", res.Error)
	} else {
		out.TaskResults = res.RowsAffected
	}
	res := tx.Where("id IN ?", ids).Delete(&Run{})
	if res.Error != nil {
		tx.Rollback()
		return out, fmt.Errorf("delete runs: %w", res.Error)
	}
	out.Runs = res.RowsAffected
	if err := tx.Commit().Error; err != nil {
		return out, fmt.Errorf("commit retention transaction: %w", err)
	}
	return out, nil
}

// PruneHosts removes hosts that are no longer in a collection's inventory
// and whose last sight is older than delay. Per-host history is deleted in
// the same transaction as the host rows.
func (d *DB) PruneHosts(ctx context.Context, target string, delay time.Duration) (int64, error) {
	if delay <= 0 {
		return 0, nil
	}

	type candidate struct {
		ID          uint64
		FirstSeenAt time.Time
		LastSeenAt  time.Time
	}
	var rows []candidate
	if err := d.db.WithContext(ctx).Model(&Host{}).
		Select("id", "first_seen_at", "last_seen_at").
		Where("collection_id IN (SELECT id FROM collections WHERE name = ?)", target).
		Where("in_inventory = ?", false).
		Scan(&rows).Error; err != nil {
		return 0, fmt.Errorf("list hosts for cleanup: %w", err)
	}

	cutoff := time.Now().UTC().Add(-delay)
	ids := make([]uint64, 0, len(rows))
	for _, h := range rows {
		lastSeen := h.LastSeenAt
		if lastSeen.IsZero() {
			lastSeen = h.FirstSeenAt
		}
		if !lastSeen.IsZero() && lastSeen.Before(cutoff) {
			ids = append(ids, h.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}

	tx := d.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return 0, fmt.Errorf("begin host cleanup transaction: %w", tx.Error)
	}
	for _, table := range []struct {
		name  string
		model any
	}{
		{"host runs", &HostRun{}},
		{"task results", &TaskRunResult{}},
		{"task states", &TaskState{}},
		{"ansible runs", &AnsibleRun{}},
	} {
		if err := tx.Where("host_id IN ?", ids).Delete(table.model).Error; err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("delete %s: %w", table.name, err)
		}
	}
	res := tx.Where("id IN ?", ids).Delete(&Host{})
	if res.Error != nil {
		tx.Rollback()
		return 0, fmt.Errorf("delete hosts: %w", res.Error)
	}
	if err := tx.Commit().Error; err != nil {
		return 0, fmt.Errorf("commit host cleanup transaction: %w", err)
	}
	return res.RowsAffected, nil
}

func (d *DB) Stats(ctx context.Context) (*store.Stats, error) {
	st := &store.Stats{PerPlaybook: []store.PlaybookStats{}}

	{
		// A hostname may have one row per playbook/inventory context, but the
		// dashboard counts each (target, hostname) once.
		row := struct {
			Total  int64
			InInv  int64
			Active int64
			Gone   int64
		}{}
		err := d.db.WithContext(ctx).Model(&Host{}).
			Joins("JOIN collections c ON c.id = hosts.collection_id").
			Select(`COUNT(DISTINCT (c.name, hosts.name)) AS total,
				COUNT(DISTINCT (c.name, hosts.name)) FILTER (WHERE hosts.in_inventory) AS in_inv,
				COUNT(DISTINCT (c.name, hosts.name)) FILTER (WHERE hosts.active) AS active,
				COUNT(DISTINCT (c.name, hosts.name)) FILTER (WHERE hosts.in_inventory AND NOT hosts.active) AS gone`).
			Scan(&row).Error
		if err != nil {
			return nil, err
		}
		st.Hosts = store.HostsStats{
			Total:       int(row.Total),
			InInventory: int(row.InInv),
			Active:      int(row.Active),
			Disappeared: int(row.Gone),
		}
	}

	{
		row := struct {
			Total     int64
			Succeeded int64
			Failed    int64
			AvgDur    *float64
			Last      *time.Time
		}{}
		if err := d.db.WithContext(ctx).Model(&Run{}).
			Select("COUNT(*) AS total, " +
				"COUNT(*) FILTER (WHERE success) AS succeeded, " +
				"COUNT(*) FILTER (WHERE NOT success) AS failed, " +
				"AVG(duration_ms) AS avg_dur, " +
				"MAX(finished_at) AS last").
			Scan(&row).Error; err != nil {
			return nil, err
		}
		st.Runs = store.RunsStats{
			Total:     int(row.Total),
			Succeeded: int(row.Succeeded),
			Failed:    int(row.Failed),
		}
		if row.AvgDur != nil {
			st.Runs.AvgDurationMS = int64(*row.AvgDur)
		}
		if row.Last != nil {
			st.Runs.LastRunAt = *row.Last
		}
	}

	{
		// GORM leaves the slice nil when there are no rows; keep it non-nil
		// so the API serializes an empty array instead of null (the UI
		// expects an array here).
		pbs := make([]store.PlaybookStats, 0)
		err := d.db.WithContext(ctx).Model(&Run{}).
			Select("target, playbook, COUNT(*) AS total, " +
				"COUNT(*) FILTER (WHERE NOT success) AS failed, " +
				"MAX(finished_at) AS last_run_at").
			Group("target, playbook").
			Order("target, playbook").
			Scan(&pbs).Error
		if err != nil {
			return nil, err
		}
		st.PerPlaybook = pbs
	}

	return st, nil
}

func (d *DB) Close() error {
	sqlDB, err := d.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func toRunModel(r store.RunRecord) Run {
	return Run{
		ID:           r.ID,
		Target:       r.Target,
		Playbook:     r.Playbook,
		StartedAt:    r.StartedAt,
		FinishedAt:   r.FinishedAt,
		DurationMS:   r.DurationMS,
		ExitCode:     r.ExitCode,
		Success:      r.Success,
		Hosts:        r.Hosts,
		ChangedHosts: r.ChangedHosts,
		FailedHosts:  r.FailedHosts,
		Output:       r.Output,
		State:        r.State,
		Canceled:     r.Canceled,
		Check:        r.Check,
		JobID:        r.JobID,
		Inventory:    r.Inventory,
	}
}

func fromRunModel(r Run) store.RunRecord {
	return store.RunRecord{
		ID:           r.ID,
		Target:       r.Target,
		Playbook:     r.Playbook,
		StartedAt:    r.StartedAt,
		FinishedAt:   r.FinishedAt,
		DurationMS:   r.DurationMS,
		ExitCode:     r.ExitCode,
		Success:      r.Success,
		Hosts:        r.Hosts,
		ChangedHosts: r.ChangedHosts,
		FailedHosts:  r.FailedHosts,
		Output:       r.Output,
		State:        r.State,
		Canceled:     r.Canceled,
		Check:        r.Check,
		JobID:        r.JobID,
		Inventory:    r.Inventory,
	}
}

func toHostRunModel(run Run, hostID uint64, h store.HostRunRecord) HostRun {
	return HostRun{
		RunID:  run.ID,
		HostID: hostID,
		Status: h.Status,
		TaskCounters: TaskCounters{
			Ok: h.Ok, Changed: h.Changed, Failed: h.Failed,
			Unreachable: h.Unreachable, Skipped: h.Skipped,
			Rescued: h.Rescued, Ignored: h.Ignored,
		},
	}
}

func fromHostModel(h Host, target string) store.HostRecord {
	groups := []string{}
	for _, g := range strings.Split(h.Groups, ",") {
		if g != "" {
			groups = append(groups, g)
		}
	}
	return store.HostRecord{
		Target:        target,
		Name:          h.Name,
		IP:            h.IP,
		User:          h.User,
		Groups:        groups,
		InInventory:   h.InInventory,
		Active:        h.Active,
		FirstSeenAt:   h.FirstSeenAt,
		LastSeenAt:    h.LastSeenAt,
		LastRunAt:     h.LastRunAt,
		LastStatus:    h.LastStatus,
		LastChangeAt:  h.LastChangeAt,
		DisappearedAt: h.DisappearedAt,
		Facts:         json.RawMessage(h.Facts),
	}
}
