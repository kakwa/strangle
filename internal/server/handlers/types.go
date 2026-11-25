package handlers

import (
	"time"

	"github.com/kakwa/strangle/internal/auth"
)

// Response types shared by handlers and used by swag annotations.
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

// The types below mirror the bodies decoded/written by the handlers so the
// swag annotations can refer to stable names. They are not constructed at
// runtime except where noted.
type apiErrorDoc struct {
	Error string `json:"error"`
}

type messageResponseDoc struct {
	Status string `json:"status"`
}

type meResponseDoc struct {
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
}

type loginRequestDoc struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type tokenCreateRequestDoc struct {
	Name     string `json:"name"`
	TTLHours int64  `json:"ttl_hours,omitempty"`
}

type tokenCreateResponseDoc struct {
	Raw   string     `json:"raw"`
	Token auth.Token `json:"token"`
}

type orgCreateRequestDoc struct {
	Name string `json:"name"`
}

type orgCreateResponseDoc struct {
	Name        string           `json:"name"`
	Collections []collectionInfo `json:"collections"`
}

type collectionDetailResponseDoc struct {
	Name      string            `json:"name"`
	Org       string            `json:"org"`
	Playbooks []playbookInfo    `json:"playbooks"`
	Links     map[string]string `json:"links"`
}

type syncRequestDoc struct {
	Branch string `json:"branch,omitempty"`
}

type syncResponseDoc struct {
	Target  string `json:"target"`
	Rev     string `json:"rev"`
	Changed bool   `json:"changed"`
}

// jsonPatchOperationDoc is one RFC 6902 operation. Only `replace`/`add` on
// /status and /fingerprint are accepted by the host status handler.
type jsonPatchOperationDoc struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

type hostStatusResponseDoc struct {
	Status string `json:"status"`
	Host   string `json:"host"`
}
