package store

// HostRun is a host's recap line for one run, joined with the run in the
// query APIs. The counters are shared with the ansible_runs stats blob via
// TaskCounters.
type HostRun struct {
	RunID  uint64 `gorm:"primaryKey"`
	HostID uint64 `gorm:"primaryKey"`
	Status string
	TaskCounters
}
