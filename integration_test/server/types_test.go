package server_test

import "time"

// Local mirrors of the server's JSON response shapes. The integration tests
// decode into these instead of depending on unexported server types.
type orgInfo struct {
	Name        string           `json:"name"`
	Collections []collectionInfo `json:"collections"`
}

type collectionInfo struct {
	Name      string         `json:"name"`
	Org       string         `json:"org"`
	Playbooks []playbookInfo `json:"playbooks,omitempty"`
}

type playbookInfo struct {
	Name       string     `json:"name"`
	Playbook   string     `json:"playbook"`
	Inventory  string     `json:"inventory"`
	LastStatus string     `json:"last_status,omitempty"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
}

type meResponse struct {
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
}
