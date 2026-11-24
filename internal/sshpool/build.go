package sshpool

import (
	"fmt"

	"github.com/kakwa/strangle/internal/config"
)

// BuildMulti creates one SSH manager per playbook that enables the Go-managed
// SSH transport, and a dispatcher over them. It returns nil when no playbook
// uses the transport, in which case no connection plugin is installed.
//
// Both the master (for the host-key approval API) and every worker (for the
// connections themselves) build their own Multi from the same target
// configuration; the host-key decisions are shared through the per-scope
// files on disk.
func BuildMulti(targets []config.TargetConfig) (*Multi, error) {
	managers := map[string]*Manager{}
	for _, t := range targets {
		for _, pb := range t.Playbooks {
			if !pb.Options.SSH.Enabled {
				continue
			}
			mgr, err := New(pb.Options.SSH)
			if err != nil {
				return nil, fmt.Errorf("target %q playbook %q: %w", t.Name, pb.Name, err)
			}
			managers[pb.SSHKey()] = mgr
		}
	}
	return NewMulti(managers)
}
