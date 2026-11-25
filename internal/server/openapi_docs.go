package server

import (
	"github.com/kakwa/strangle/internal/auth"
	"github.com/kakwa/strangle/internal/store"
)

const (
	jsonMedia = "application/json"
	htmlMedia = "text/html"
)

// The types below describe request and response bodies in the generated
// OpenAPI document. They mirror the anonymous structs decoded/written by the
// handlers so the swag annotations can refer to stable names. They are never
// constructed at runtime.
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

type runRequestDoc struct {
	Target   string `json:"target"`
	Playbook string `json:"playbook"`
	Check    bool   `json:"check,omitempty"`
}

type runAcceptedResponseDoc struct {
	Status   string `json:"status"`
	Target   string `json:"target"`
	Playbook string `json:"playbook"`
	RunID    uint64 `json:"run_id"`
	Check    bool   `json:"check"`
}

type syncRequestDoc struct {
	Target string `json:"target"`
	Branch string `json:"branch,omitempty"`
}

type syncResponseDoc struct {
	Target  string `json:"target"`
	Rev     string `json:"rev"`
	Changed bool   `json:"changed"`
}

type cancelRunResponseDoc struct {
	ID    uint64 `json:"id"`
	State string `json:"state"`
}

type hostKeyDecisionRequestDoc struct {
	Target      string `json:"target"`
	Host        string `json:"host"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

type hostKeyDecisionResponseDoc struct {
	Status string `json:"status"`
	Host   string `json:"host"`
}

type messageResponseDoc struct {
	Status string `json:"status"`
}

type meResponseDoc struct {
	Authenticated bool   `json:"authenticated"`
	Username      string `json:"username,omitempty"`
}

type collectionDetailResponseDoc struct {
	Name      string            `json:"name"`
	Org       string            `json:"org"`
	Playbooks []playbookInfo    `json:"playbooks"`
	Links     map[string]string `json:"links"`
}

type apiErrorDoc struct {
	Error string `json:"error"`
}

// runListResponseDoc is the header-bearing envelope used by the run listings.
// The response body itself is []store.RunRecord.
type runListResponseDoc struct {
	Runs       []store.RunRecord `json:"runs"`
	TotalCount int               `json:"total_count"`
}
