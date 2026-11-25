// Ansible YAML inventory directories (one file per pool, merged by Ansible)
// parsed into a flat host list with group membership.
package applier

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Host is a single host from the inventory.
type Host struct {
	// Name is the inventory hostname.
	Name string
	// IP is the value of ansible_host (may be empty).
	IP string
	// User is the value of ansible_user (may be empty).
	User string
	// Groups are the explicit groups the host belongs to, excluding "all".
	Groups []string
}

type groupNode struct {
	hosts    map[string]struct{}
	children map[string]struct{}
}

// Inventory is a parsed inventory directory.
type Inventory struct {
	// Hosts is the merged host list, sorted by name.
	Hosts []Host

	groups     map[string]*groupNode
	globalUser string
}

// Load reads every *.yml / *.yaml file in dir and merges them.
func Load(dir string) (*Inventory, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read inventory dir %q: %w", dir, err)
	}

	inv := &Inventory{groups: map[string]*groupNode{}}
	seen := map[string]*Host{}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".yml" && ext != ".yaml" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read inventory file %q: %w", e.Name(), err)
		}
		var root map[string]any
		if err := yaml.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("parse inventory file %q: %w", e.Name(), err)
		}
		// Files are usually rooted at "all: {children, hosts, vars}";
		// accept a bare {hosts, children, vars} root as well.
		if all, ok := root["all"].(map[string]any); ok {
			walk(all, nil, inv, seen)
		} else {
			walk(root, nil, inv, seen)
		}
	}

	for _, h := range seen {
		if h.User == "" {
			h.User = inv.globalUser
		}
		inv.Hosts = append(inv.Hosts, *h)
	}
	sort.Slice(inv.Hosts, func(i, j int) bool { return inv.Hosts[i].Name < inv.Hosts[j].Name })
	return inv, nil
}

// walk recurses over an inventory node. chain holds the group names from the
// "all" root down to the current node (empty at the root).
func walk(node map[string]any, chain []string, inv *Inventory, seen map[string]*Host) {
	if hosts, ok := node["hosts"].(map[string]any); ok {
		for name, rawVars := range hosts {
			h := &Host{Name: name}
			if v, ok := rawVars.(map[string]any); ok {
				if ip, ok := v["ansible_host"].(string); ok {
					h.IP = ip
				}
				if u, ok := v["ansible_user"].(string); ok {
					h.User = u
				}
			}
			h.Groups = append([]string(nil), chain...)
			if existing, ok := seen[name]; ok {
				for _, g := range chain {
					if !contains(existing.Groups, g) {
						existing.Groups = append(existing.Groups, g)
					}
				}
			} else {
				seen[name] = h
			}
			for _, g := range chain {
				gn := inv.groups[g]
				if gn == nil {
					gn = &groupNode{hosts: map[string]struct{}{}, children: map[string]struct{}{}}
					inv.groups[g] = gn
				}
				gn.hosts[name] = struct{}{}
			}
		}
	}

	if vars, ok := node["vars"].(map[string]any); ok && len(chain) == 0 {
		if u, ok := vars["ansible_user"].(string); ok {
			inv.globalUser = u
		}
	}

	if children, ok := node["children"].(map[string]any); ok {
		for name, rawChild := range children {
			gn := inv.groups[name]
			if gn == nil {
				gn = &groupNode{hosts: map[string]struct{}{}, children: map[string]struct{}{}}
				inv.groups[name] = gn
			}
			if len(chain) > 0 {
				inv.groups[chain[len(chain)-1]].children[name] = struct{}{}
			}
			child, _ := rawChild.(map[string]any)
			walk(child, append(append([]string(nil), chain...), name), inv, seen)
		}
	}
}

// Members returns the host names contained in a group or named directly,
// including members of child groups. Patterns that match nothing (e.g. a
// group absent from the inventory) yield an empty slice.
func (inv *Inventory) Members(patterns ...string) []string {
	out := map[string]struct{}{}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == "all" || p == "*" {
			for _, h := range inv.Hosts {
				out[h.Name] = struct{}{}
			}
			continue
		}
		if gn, ok := inv.groups[p]; ok {
			for h := range inv.members(gn) {
				out[h] = struct{}{}
			}
			continue
		}
		// A plain hostname pattern.
		for _, h := range inv.Hosts {
			if h.Name == p {
				out[p] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(out))
	for n := range out {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// members resolves a group's hosts including descendant groups.
func (inv *Inventory) members(gn *groupNode) map[string]struct{} {
	if gn == nil {
		return map[string]struct{}{}
	}
	// Walk by identity to avoid infinite loops on malformed inventories.
	seen := map[*groupNode]struct{}{gn: {}}
	return inv.collect(gn, seen)
}

func (inv *Inventory) collect(gn *groupNode, seen map[*groupNode]struct{}) map[string]struct{} {
	out := map[string]struct{}{}
	for h := range gn.hosts {
		out[h] = struct{}{}
	}
	for child := range gn.children {
		cgn := inv.groups[child]
		if cgn == nil {
			continue
		}
		if _, ok := seen[cgn]; ok {
			continue
		}
		seen[cgn] = struct{}{}
		for h := range inv.collect(cgn, seen) {
			out[h] = struct{}{}
		}
	}
	return out
}

func contains(groups []string, g string) bool {
	for _, x := range groups {
		if x == g {
			return true
		}
	}
	return false
}
