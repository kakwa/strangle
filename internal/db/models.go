package db

import "encoding/json"

// TaskCounters is the recap shared by the host_runs table and the stats blob
// of ansible_runs. Keeping it in one place avoids two drifting copies of the
// same seven counters.
type TaskCounters struct {
	Ok          int `gorm:"column:ok" json:"ok"`
	Changed     int `gorm:"column:changed" json:"changed"`
	Failed      int `gorm:"column:failed" json:"failed"`
	Unreachable int `gorm:"column:unreachable" json:"unreachable"`
	Skipped     int `gorm:"column:skipped" json:"skipped"`
	Rescued     int `gorm:"column:rescued" json:"rescued"`
	Ignored     int `gorm:"column:ignored" json:"ignored"`
}

// AnsibleTask is one entry of an AnsibleRun's Tasks jsonb array.
type AnsibleTask struct {
	Task   string `json:"task"`
	Status string `json:"status"`
	Diff   string `json:"diff,omitempty"`
	Error  string `json:"error,omitempty"`
}

// AnsibleStats is the jsonb stats blob of an AnsibleRun. Run-level metadata
// (mode, status, timing, success) lives on runs; only the recap is repeated
// per host.
type AnsibleStats struct {
	TaskCounters
}

func jsonBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}
