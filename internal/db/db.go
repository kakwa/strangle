// Package db implements the store.Store interface on PostgreSQL via gorm.
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/parser"
	"github.com/kakwa/strangle/internal/store"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Run is one playbook execution.
type Run struct {
	ID           uint64 `gorm:"primaryKey"`
	Target       string `gorm:"size:255;index"`
	Playbook     string `gorm:"size:512;index"`
	StartedAt    time.Time
	FinishedAt   time.Time `gorm:"index"`
	DurationMS   int64
	ExitCode     int
	Success      bool
	Hosts        int
	ChangedHosts int
	FailedHosts  int
	Output       string `gorm:"type:text"`
}

// Host is the current state of a host within one target.
type Host struct {
	ID            uint64 `gorm:"primaryKey"`
	Target        string `gorm:"size:255;uniqueIndex:idx_host_target_name"`
	Name          string `gorm:"size:255;uniqueIndex:idx_host_target_name"`
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
}

// HostRun is a host's recap line for one run.
type HostRun struct {
	RunID       uint64 `gorm:"primaryKey"`
	HostID      uint64 `gorm:"primaryKey"`
	Status      string
	Ok          int
	Changed     int
	Failed      int
	Unreachable int
	Skipped     int
	Rescued     int
	Ignored     int
}

// TaskState is the latest known state of one task on one host.
type TaskState struct {
	ID            uint64 `gorm:"primaryKey"`
	HostID        uint64 `gorm:"uniqueIndex:idx_taskstate_host_task"`
	Task          string `gorm:"size:512;uniqueIndex:idx_taskstate_host_task"`
	LastRunID     uint64
	LastRunAt     time.Time
	LastStatus    string
	LastChangedAt *time.Time
	LastDiff      string `gorm:"type:text"`
	LastError     string
}

// TaskRunResult is one task result of one run. It is append-only: task_states
// keeps only the latest state per host/task, while this table preserves every
// run's status, diff and error (including repeated task names).
type TaskRunResult struct {
	ID         uint64 `gorm:"primaryKey"`
	RunID      uint64 `gorm:"index"`
	HostID     uint64
	Task       string `gorm:"size:512"`
	Status     string
	Diff       string `gorm:"type:text"`
	Error      string
	FinishedAt time.Time
}

// DB is a store.Store backed by PostgreSQL.
type DB struct {
	db *gorm.DB
}

// Open connects to the PostgreSQL database described by dsn and migrates the
// schema.
func Open(ctx context.Context, dsn string) (*DB, error) {
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
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

	if err := gdb.WithContext(ctx).AutoMigrate(&Run{}, &Host{}, &HostRun{}, &TaskState{}, &TaskRunResult{}, &UserRecord{}, &TokenRecord{}, &CollectionMemberRecord{}); err != nil {
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	// AutoMigrate never drops indexes. Remove the legacy unique index on
	// hosts.name from single-target builds; hosts are now scoped per target
	// (idx_host_target_name).
	if err := gdb.WithContext(ctx).Exec("DROP INDEX IF EXISTS idx_hosts_name").Error; err != nil {
		return nil, fmt.Errorf("drop legacy hosts index: %w", err)
	}
	return &DB{db: gdb}, nil
}

func (d *DB) SaveRun(ctx context.Context, in store.RunInput) error {
	return d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		run := toRunModel(in.Run)
		if err := tx.Create(&run).Error; err != nil {
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

		target := in.Run.Target

		// Hosts are scoped per target: every lookup is qualified with the
		// run's target so names can repeat across targets.
		hostIDs := map[string]uint64{}
		if len(nameList) > 0 {
			var existing []Host
			if err := tx.Where("target = ? AND name IN ?", target, nameList).Find(&existing).Error; err != nil {
				return err
			}
			for _, h := range existing {
				hostIDs[h.Name] = h.ID
			}
			for _, n := range nameList {
				if _, ok := hostIDs[n]; ok {
					continue
				}
				h := Host{Target: target, Name: n, FirstSeenAt: run.FinishedAt}
				if err := tx.Create(&h).Error; err != nil {
					return fmt.Errorf("insert host %q: %w", n, err)
				}
				hostIDs[n] = h.ID
			}
		}

		// Inventory snapshot: update known fields and membership.
		for _, ih := range in.Inventory {
			updates := map[string]any{
				"ip":           ih.IP,
				"user":         ih.User,
				"groups":       strings.Join(ih.Groups, ","),
				"in_inventory": true,
			}
			if err := tx.Model(&Host{}).Where("target = ? AND name = ?", target, ih.Name).Updates(updates).Error; err != nil {
				return err
			}
		}
		if len(in.Inventory) > 0 {
			invNames := make([]string, 0, len(in.Inventory))
			for _, ih := range in.Inventory {
				invNames = append(invNames, ih.Name)
			}
			if err := tx.Model(&Host{}).Where("target = ? AND name NOT IN ?", target, invNames).
				UpdateColumn("in_inventory", false).Error; err != nil {
				return err
			}
		}

		// Recap lines.
		inRecap := map[string]struct{}{}
		for _, h := range in.Hosts {
			inRecap[h.Host] = struct{}{}
			status := parser.Status(h.Ok, h.Changed, h.Failed, h.Unreachable)
			updates := map[string]any{
				"last_seen_at":   run.FinishedAt,
				"last_run_at":    run.FinishedAt,
				"last_status":    status,
				"active":         true,
				"disappeared_at": nil,
			}
			if err := tx.Model(&Host{}).Where("id = ?", hostIDs[h.Host]).Updates(updates).Error; err != nil {
				return err
			}
			hr := toHostRunModel(run, hostIDs[h.Host], h)
			if err := tx.Create(&hr).Error; err != nil {
				return fmt.Errorf("insert host run: %w", err)
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
	var hosts []Host
	if err := d.db.WithContext(ctx).Order("target, name").Find(&hosts).Error; err != nil {
		return nil, err
	}
	out := make([]store.HostRecord, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, fromHostModel(h))
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
		Joins("JOIN runs r ON r.id = host_runs.run_id").
		Select("host_runs.*, h.name AS hostname, h.target AS hosttarget, r.playbook, r.started_at, r.duration_ms, r.exit_code, r.success").
		Where("h.name = ?", host).
		Order("host_runs.run_id DESC")
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
	q := d.db.WithContext(ctx).Model(&TaskState{}).
		Joins("JOIN hosts h ON h.id = task_states.host_id").
		Select("task_states.*, h.name AS hostname, h.target AS hosttarget").
		Where("h.name = ?", host).
		Order("COALESCE(task_states.last_changed_at, task_states.last_run_at) DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
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
		Select("task_run_results.*, h.name AS hostname, h.target AS hosttarget").
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

func (d *DB) Stats(ctx context.Context) (*store.Stats, error) {
	st := &store.Stats{PerPlaybook: []store.PlaybookStats{}}

	{
		// Each count is built from a fresh statement: reusing one *gorm.DB
		// across queries accumulates WHERE clauses.
		var total, inInv, active, gone int64
		if err := d.db.WithContext(ctx).Model(&Host{}).
			Count(&total).Error; err != nil {
			return nil, err
		}
		if err := d.db.WithContext(ctx).Model(&Host{}).
			Where("in_inventory").Count(&inInv).Error; err != nil {
			return nil, err
		}
		if err := d.db.WithContext(ctx).Model(&Host{}).
			Where("active").Count(&active).Error; err != nil {
			return nil, err
		}
		if err := d.db.WithContext(ctx).Model(&Host{}).
			Where("in_inventory AND NOT active").
			Count(&gone).Error; err != nil {
			return nil, err
		}
		st.Hosts = store.HostsStats{
			Total:       int(total),
			InInventory: int(inInv),
			Active:      int(active),
			Disappeared: int(gone),
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
	}
}

func toHostRunModel(run Run, hostID uint64, h store.HostRunRecord) HostRun {
	return HostRun{
		RunID:       run.ID,
		HostID:      hostID,
		Status:      h.Status,
		Ok:          h.Ok,
		Changed:     h.Changed,
		Failed:      h.Failed,
		Unreachable: h.Unreachable,
		Skipped:     h.Skipped,
		Rescued:     h.Rescued,
		Ignored:     h.Ignored,
	}
}

func fromHostModel(h Host) store.HostRecord {
	groups := []string{}
	for _, g := range strings.Split(h.Groups, ",") {
		if g != "" {
			groups = append(groups, g)
		}
	}
	return store.HostRecord{
		Target:        h.Target,
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
	}
}
