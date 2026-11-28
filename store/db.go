// Package store implements the Store interface on PostgreSQL or SQLite via
// gorm. The flavor is selected from the DSN at Open time (see dialectorFor),
// so a single build serves either backend.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kakwa/strangle/cfg"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB is a Store backed by PostgreSQL or SQLite.
type DB struct {
	gdb *gorm.DB
}

// Open connects to the database described by dsn and migrates the schema.
// The backend is chosen from the DSN: a postgres:// (or postgresql://) URI
// opens PostgreSQL, anything else (a file path, a file: URI, or a
// sqlite:// URI) opens SQLite.
func Open(ctx context.Context, dsn string) (*DB, error) {
	d, err := dialectorFor(dsn)
	if err != nil {
		return nil, fmt.Errorf("resolve database dialect: %w", err)
	}
	gdb, err := gorm.Open(d.open, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
		// Turn unique-constraint violations into gorm.ErrDuplicatedKey so
		// stores can map duplicate users/tokens to their domain errors
		// (SQLSTATE 23505 on PostgreSQL, SQLITE_CONSTRAINT on SQLite).
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
		&RunRecord{}, &HostRun{}, &TaskState{}, &TaskRunResult{}, &TokenRecord{}, &CollectionMemberRecord{},
	); err != nil {
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	// AutoMigrate never drops indexes. On an upgraded PostgreSQL install,
	// replace the old per-target host index with the context key (collection,
	// hostname, playbook, inventory), and the old unscoped token index with
	// one that includes the token's scope. Fresh SQLite databases have none
	// of these, so the step is skipped there.
	if d.name == "postgres" {
		for _, idx := range []string{"idx_hosts_name", "idx_host_target_name", "idx_api_tokens_owner_name"} {
			if err := gdb.WithContext(ctx).Exec("DROP INDEX IF EXISTS " + idx).Error; err != nil {
				return nil, fmt.Errorf("drop legacy index %s: %w", idx, err)
			}
		}
	}

	// Every installation has the default org, even before the collection
	// file is loaded. Collections without an explicit org belong to it.
	if err := gdb.WithContext(ctx).Where("name = ?", cfg.DefaultOrg).
		FirstOrCreate(&Org{}, Org{Name: cfg.DefaultOrg}).Error; err != nil {
		return nil, fmt.Errorf("initialize default org: %w", err)
	}
	return &DB{gdb: gdb}, nil
}

// runIDCounter allocates run IDs before the database assigns them. Runs are
// created before their queue job is inserted, so the ID must come from here.
var runIDCounter atomic.Uint64

func init() {
	runIDCounter.Store(uint64(time.Now().UnixNano()))
}

func (d *DB) CreateRun(ctx context.Context, in *RunRecord) error {
	if in.ID == 0 {
		in.ID = runIDCounter.Add(1)
	}
	if in.StartedAt.IsZero() {
		in.StartedAt = time.Now().UTC()
	}
	if in.State == "" {
		in.State = RunStateQueued
	}
	if err := d.gdb.WithContext(ctx).Create(in).Error; err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

// runUpdates lists every mutable run column. The map form updates zero
// values too, which is what finalizing a run needs.
func runUpdates(r RunRecord) map[string]any {
	return map[string]any{
		"target": r.Target, "playbook": r.Playbook, "started_at": r.StartedAt,
		"finished_at": r.FinishedAt, "duration_ms": r.DurationMS, "exit_code": r.ExitCode,
		"success": r.Success, "hosts": r.Hosts, "changed_hosts": r.ChangedHosts,
		"failed_hosts": r.FailedHosts, "output": r.Output, "state": r.State,
		"canceled": r.Canceled, "check": r.Check, "job_id": r.JobID, "inventory": r.Inventory,
	}
}

func (d *DB) UpdateRun(ctx context.Context, in RunRecord) error {
	res := d.gdb.WithContext(ctx).Model(&RunRecord{}).Where("id = ?", in.ID).Updates(runUpdates(in))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) SaveRun(ctx context.Context, in RunInput) error {
	// A run normally gets its ID from CreateRun before its queue job is
	// inserted. Allocate one here too so inserting a completed run directly
	// cannot collide with IDs assigned earlier in the same database.
	if in.Run.ID == 0 {
		in.Run.ID = runIDCounter.Add(1)
	}
	return d.gdb.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The ID was allocated above, so the run exists only when CreateRun
		// made it for the queue job; finalize it, inserting a direct run.
		run := in.Run
		var existing RunRecord
		switch err := tx.Where("id = ?", run.ID).First(&existing).Error; {
		case err == nil:
			// The job ID is written by the enqueuer after the job is
			// inserted and can race with a fast worker; never clear it
			// when finalizing a run.
			if run.JobID == "" {
				run.JobID = existing.JobID
			}
			if existing.Canceled {
				run.State = RunStateCancelled
				run.Canceled = true
			}
			if err := tx.Model(&RunRecord{}).Where("id = ?", run.ID).Updates(runUpdates(run)).Error; err != nil {
				return fmt.Errorf("update run: %w", err)
			}
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		default:
			if err := tx.Create(&run).Error; err != nil {
				return fmt.Errorf("insert run: %w", err)
			}
		}

		// A job is retried when the playbook fails, and its results are saved
		// before the failure is returned. Finalizing the same run ID again
		// must replace the earlier attempt's per-run rows: host_runs and
		// ansible_runs are keyed by run ID, so a plain insert collides. Task
		// states are the latest-view table and are updated in place below.
		for _, model := range []any{&HostRun{}, &AnsibleRun{}, &TaskRunResult{}} {
			if err := tx.Where("run_id = ?", run.ID).Delete(model).Error; err != nil {
				return fmt.Errorf("clear previous run results: %w", err)
			}
		}

		// Collect every host name involved so IDs can be resolved once.
		names := map[string]struct{}{}
		for _, h := range in.Hosts {
			names[h.Host] = struct{}{}
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

		// Per-host run history: recap counters and task results as jsonb.
		// Everything else (org, collection, target, mode, timing) is reached
		// via the host and the aggregate run row, so it is not stored twice.
		tasksByHost := map[string][]AnsibleTask{}
		for _, t := range in.Tasks {
			tasksByHost[t.Host] = append(tasksByHost[t.Host], AnsibleTask{
				Task: t.Task, Status: t.Status, Diff: t.LastDiff, Error: t.LastError,
			})
		}
		for _, h := range in.Hosts {
			status := "ok"
			switch {
			case h.Failed > 0:
				status = "failed"
			case h.Unreachable > 0:
				status = "unreachable"
			case h.Changed > 0:
				status = "changed"
			}
			updates := map[string]any{
				"last_seen_at": run.FinishedAt,
				"last_run_at":  run.FinishedAt,
				"last_status":  status,
			}
			if facts := in.Facts[h.Host]; len(facts) > 0 {
				updates["facts"] = []byte(facts)
			}
			if err := tx.Model(&Host{}).Where("id = ?", hostIDs[h.Host]).Updates(updates).Error; err != nil {
				return err
			}
			hr := toHostRunModel(run.ID, hostIDs[h.Host], h)
			if err := tx.Create(&hr).Error; err != nil {
				return fmt.Errorf("insert host run: %w", err)
			}
			ar := AnsibleRun{
				RunID:  run.ID,
				HostID: hostIDs[h.Host],
				Tasks:  jsonBytes(tasksByHost[h.Host]),
				Stats:  jsonBytes(AnsibleStats{TaskCounters: h.TaskCounters}),
			}
			if err := tx.Create(&ar).Error; err != nil {
				return fmt.Errorf("insert ansible run: %w", err)
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
				ts = TaskState{HostID: hostID, Task: t.Task}
			} else if err != nil {
				return err
			}
			ts.LastRunID = run.ID
			ts.LastRunAt = run.FinishedAt
			ts.LastStatus = t.Status
			updates := map[string]any{
				"last_run_id": run.ID,
				"last_run_at": run.FinishedAt,
				"last_status": t.Status,
			}
			if t.Status == "changed" {
				ts.LastChangedAt = &run.FinishedAt
				ts.LastDiff = t.LastDiff
				updates["last_changed_at"] = run.FinishedAt
				updates["last_diff"] = t.LastDiff
				changedHosts[hostID] = struct{}{}
			}
			if t.Status == "failed" {
				ts.LastError = t.LastError
				updates["last_error"] = t.LastError
			}
			if ts.ID == 0 {
				if err := tx.Create(&ts).Error; err != nil {
					return fmt.Errorf("insert task state: %w", err)
				}
			} else if err := tx.Model(&TaskState{}).Where("id = ?", ts.ID).Updates(updates).Error; err != nil {
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

func (d *DB) ListRuns(ctx context.Context, limit int) ([]RunRecord, error) {
	var runs []RunRecord
	q := d.gdb.WithContext(ctx).Order("id DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

func (d *DB) Run(ctx context.Context, id uint64) (RunRecord, error) {
	var run RunRecord
	if err := d.gdb.WithContext(ctx).First(&run, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return RunRecord{}, fmt.Errorf("run %d: %w", id, ErrNotFound)
		}
		return RunRecord{}, err
	}
	return run, nil
}

func (d *DB) ListHosts(ctx context.Context) ([]HostRecord, error) {
	// The collection name is the target exposed by the APIs; it is not stored
	// on hosts anymore, so the host view joins collections to get it.
	type hostRow struct {
		Host
		Target string `gorm:"column:target"`
	}
	var hosts []hostRow
	// The same hostname can have one row per playbook/inventory context; the
	// host view only shows its latest appearance per collection. ROW_NUMBER is
	// used instead of PostgreSQL's SELECT DISTINCT ON so the query is portable
	// to SQLite. (last_run_at IS NULL) DESC mirrors PostgreSQL's default
	// NULLS-FIRST ordering for a DESC column.
	if err := d.gdb.WithContext(ctx).
		Raw(`SELECT h.*
			FROM (
				SELECT h.*, c.name AS target,
					ROW_NUMBER() OVER (
						PARTITION BY c.name, h.name
						ORDER BY (h.last_run_at IS NULL) DESC, h.last_run_at DESC, h.id DESC
					) AS rn
				FROM hosts h
				JOIN collections c ON c.id = h.collection_id
			) h
			WHERE h.rn = 1
			ORDER BY h.target, h.name`).
		Scan(&hosts).Error; err != nil {
		return nil, err
	}
	out := make([]HostRecord, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, fromHostModel(h.Host, h.Target))
	}
	return out, nil
}

func (d *DB) HostRuns(ctx context.Context, host string, limit int) ([]HostRunRecord, error) {
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
	// host_runs is keyed by (run_id, host_id), so a given host name has at
	// most one row per run; no dedup is needed. A plain SELECT (rather than
	// PostgreSQL's DISTINCT ON (r.id)) keeps the query portable to SQLite.
	q := d.gdb.WithContext(ctx).Model(&HostRun{}).
		Joins("JOIN hosts h ON h.id = host_runs.host_id").
		Joins("JOIN collections c ON c.id = h.collection_id").
		Joins("JOIN runs r ON r.id = host_runs.run_id").
		Select("host_runs.*, h.name AS hostname, c.name AS hosttarget, r.playbook, r.started_at, r.duration_ms, r.exit_code, r.success").
		Where("h.name = ?", host).
		Order("r.id DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]HostRunRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, HostRunRecord{
			RunID: r.RunID, Target: r.HostTarget, Host: r.HostName, Playbook: r.Playbook,
			StartedAt: r.StartedAt, DurationMS: r.DurationMS, ExitCode: r.ExitCode, Success: r.Success,
			Status: r.Status, TaskCounters: r.TaskCounters,
		})
	}
	return out, nil
}

func (d *DB) HostChanges(ctx context.Context, host string, limit int) ([]TaskStateRecord, error) {
	type row struct {
		TaskState
		HostName   string `gorm:"column:hostname"`
		HostTarget string `gorm:"column:hosttarget"`
	}
	var rows []row
	if err := d.gdb.WithContext(ctx).Model(&TaskState{}).
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
	when := func(r row) time.Time {
		if r.LastChangedAt != nil {
			return *r.LastChangedAt
		}
		return r.LastRunAt
	}
	sort.Slice(rows, func(i, j int) bool { return when(rows[i]).After(when(rows[j])) })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]TaskStateRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, TaskStateRecord{
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

func (d *DB) RunTasks(ctx context.Context, runID uint64) ([]TaskRunRecord, error) {
	var run RunRecord
	if err := d.gdb.WithContext(ctx).Select("id").First(&run, runID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("run %d: %w", runID, ErrNotFound)
		}
		return nil, err
	}

	type row struct {
		TaskRunResult
		HostName   string `gorm:"column:hostname"`
		HostTarget string `gorm:"column:hosttarget"`
	}
	var rows []row
	if err := d.gdb.WithContext(ctx).Model(&TaskRunResult{}).
		Joins("JOIN hosts h ON h.id = task_run_results.host_id").
		Joins("JOIN collections c ON c.id = h.collection_id").
		Select("task_run_results.*, h.name AS hostname, c.name AS hosttarget").
		Where("task_run_results.run_id = ?", runID).
		Order("task_run_results.id").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]TaskRunRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, TaskRunRecord{
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
func (d *DB) PruneRuns(ctx context.Context, target string, policy RetentionPolicy) (PruneResult, error) {
	var out PruneResult
	if !policy.Enabled() {
		return out, nil
	}

	type candidate struct {
		ID         uint64
		StartedAt  time.Time
		FinishedAt time.Time
	}
	var rows []candidate
	if err := d.gdb.WithContext(ctx).Model(&RunRecord{}).
		Select("id", "started_at", "finished_at").
		Where("target = ?", target).
		Where("state NOT IN ?", []string{RunStateQueued, RunStateRunning}).
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

	tx := d.gdb.WithContext(ctx).Begin()
	if tx.Error != nil {
		return out, fmt.Errorf("begin retention transaction: %w", tx.Error)
	}
	// Latest-state rows are kept, but must not link to a deleted run.
	if err := tx.Model(&TaskState{}).Where("last_run_id IN ?", ids).
		Update("last_run_id", 0).Error; err != nil {
		tx.Rollback()
		return out, fmt.Errorf("clear task state run links: %w", err)
	}
	remove := func(name string, model any, where string) (int64, error) {
		res := tx.Where(where, ids).Delete(model)
		if res.Error != nil {
			tx.Rollback()
			return 0, fmt.Errorf("delete %s: %w", name, res.Error)
		}
		return res.RowsAffected, nil
	}
	var err error
	if out.HostRuns, err = remove("host run history", &HostRun{}, "run_id IN ?"); err != nil {
		return out, err
	}
	if out.TaskResults, err = remove("task results", &TaskRunResult{}, "run_id IN ?"); err != nil {
		return out, err
	}
	if out.Runs, err = remove("runs", &RunRecord{}, "id IN ?"); err != nil {
		return out, err
	}
	if err := tx.Commit().Error; err != nil {
		return out, fmt.Errorf("commit retention transaction: %w", err)
	}
	return out, nil
}

// PruneHosts removes hosts of one collection whose last sight in a playbook
// recap is older than delay. Per-host history is deleted in the same
// transaction as the host rows.
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
	if err := d.gdb.WithContext(ctx).Model(&Host{}).
		Select("id", "first_seen_at", "last_seen_at").
		Where("collection_id IN (SELECT id FROM collections WHERE name = ?)", target).
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

	tx := d.gdb.WithContext(ctx).Begin()
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

func (d *DB) Close() error {
	sqlDB, err := d.gdb.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

func toHostRunModel(runID, hostID uint64, h HostRunRecord) HostRun {
	return HostRun{RunID: runID, HostID: hostID, Status: h.Status, TaskCounters: h.TaskCounters}
}

func fromHostModel(h Host, target string) HostRecord {
	groups := []string{}
	for _, g := range strings.Split(h.Groups, ",") {
		if g != "" {
			groups = append(groups, g)
		}
	}
	return HostRecord{
		Target:       target,
		Name:         h.Name,
		IP:           h.IP,
		User:         h.User,
		Groups:       groups,
		FirstSeenAt:  h.FirstSeenAt,
		LastSeenAt:   h.LastSeenAt,
		LastRunAt:    h.LastRunAt,
		LastStatus:   h.LastStatus,
		LastChangeAt: h.LastChangeAt,
		Facts:        json.RawMessage(h.Facts),
	}
}
