package api

import (
	"time"

	"github.com/kakwa/strangle/security/auth"
)

// Request and response types of the versioned API. The same structs are
// bound/marshalled at runtime by the handlers and referenced by the swag
// annotations, so the documented schema and the wire format cannot drift.
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

type statusResponse struct {
	Status string `json:"status"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type orgCreateRequest struct {
	Name string `json:"name"`
}

type orgCreateResponse struct {
	Name        string           `json:"name"`
	Collections []collectionInfo `json:"collections"`
}

type collectionDetailResponse struct {
	Name      string            `json:"name"`
	Org       string            `json:"org"`
	Playbooks []playbookInfo    `json:"playbooks"`
	Links     map[string]string `json:"links"`
}

type syncRequest struct {
	Branch string `json:"branch,omitempty"`
}

type syncResponse struct {
	Target  string `json:"target"`
	Rev     string `json:"rev"`
	Changed bool   `json:"changed"`
}

type tokenCreateRequest struct {
	Name     string `json:"name"`
	TTLHours int64  `json:"ttl_hours,omitempty"`
}

type tokenCreateResponse struct {
	Raw   string     `json:"raw"`
	Token auth.Token `json:"token"`
}

// patchOperation is one RFC 6902 operation. Only `replace`/`add` on
// /status and /fingerprint are accepted by the host status handler.
type patchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

type hostStatusResponse struct {
	Status string `json:"status"`
	Host   string `json:"host"`
}
