package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kakwa/strangle/internal/config"
	"github.com/kakwa/strangle/internal/store"
)

// apiVersion is the current version of the HTTP API. All endpoints are
// served under /api/v1; the original flat /api/* routes remain registered as
// aliases so existing clients keep working.
const apiVersion = "v1"

// apiPrefix is the versioned prefix of every API route.
const apiPrefix = "/api/" + apiVersion

// allRuns is the upper bound used when an endpoint needs to look at the whole
// run history before applying its own filters (store.ListRuns has no filter
// arguments yet).
const allRuns = 1_000_000

// orgInfo is the representation of an organization in the API.
type orgInfo struct {
	Name        string           `json:"name"`
	Collections []collectionInfo `json:"collections"`
}

// collectionInfo is the representation of a collection (a configured target)
// in the API.
type collectionInfo struct {
	Name      string         `json:"name"`
	Org       string         `json:"org"`
	Playbooks []playbookInfo `json:"playbooks,omitempty"`
}

// apiHandler registers h both under a versioned path and under the legacy
// flat alias (e.g. GET /api/v1/runs and GET /api/runs). Patterns registered
// only under the versioned tree, such as the org/collection resources, use
// mux.HandleFunc directly instead.
func apiHandler(mux *http.ServeMux, method, path string, h http.HandlerFunc) {
	mux.HandleFunc(method+" "+path, h)
	if strings.HasPrefix(path, apiPrefix) {
		alias := "/api" + strings.TrimPrefix(path, apiPrefix)
		mux.HandleFunc(method+" "+alias, h)
	}
}

// orgOf returns the organization a configured collection belongs to.
func orgOf(t config.TargetConfig) string {
	if t.Org == "" {
		return config.DefaultOrg
	}
	return t.Org
}

// runFilter is the set of query parameters supported by the run listing.
// Zero-valued times mean "no bound"; Limit and Offset are applied after the
// caller has removed records the requester may not see.
type runFilter struct {
	Collection string
	Playbook   string
	Host       string
	From       time.Time
	To         time.Time
	Limit      int
	Offset     int
}

// parseRunFilter reads the run-listing query string. Times are RFC3339
// timestamps (e.g. 2026-11-24T12:00:00Z); From is inclusive and To exclusive
// so consecutive pages and time windows do not overlap at the boundary.
func parseRunFilter(r *http.Request, collection string) (runFilter, error) {
	f := runFilter{
		Collection: collection,
		Playbook:   r.URL.Query().Get("playbook"),
		Host:       r.URL.Query().Get("host"),
		Limit:      intQuery(r, "limit", 50),
		Offset:     intQuery(r, "offset", 0),
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	if value := r.URL.Query().Get("from"); value != "" {
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return f, fmt.Errorf("invalid from timestamp %q: %w", value, err)
		}
		f.From = t
	}
	if value := r.URL.Query().Get("to"); value != "" {
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return f, fmt.Errorf("invalid to timestamp %q: %w", value, err)
		}
		f.To = t
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.To.After(f.From) {
		return f, errors.New("to must be after from")
	}
	return f, nil
}

// filteredRuns applies every run-listing filter except access control and
// pagination: those are applied by the handler so the total count reflects
// only records the requester may see.
func (s *Server) filteredRuns(ctx context.Context, f runFilter) ([]store.RunRecord, error) {
	all, err := s.store.ListRuns(ctx, allRuns)
	if err != nil {
		return nil, err
	}

	var hostRuns map[uint64]bool
	if f.Host != "" {
		hostLines, err := s.store.HostRuns(ctx, f.Host, allRuns)
		if err != nil {
			return nil, err
		}
		hostRuns = make(map[uint64]bool, len(hostLines))
		for _, line := range hostLines {
			if f.Collection == "" || line.Target == f.Collection {
				hostRuns[line.RunID] = true
			}
		}
	}

	out := make([]store.RunRecord, 0, len(all))
	for _, run := range all {
		if f.Collection != "" && run.Target != f.Collection {
			continue
		}
		if f.Playbook != "" && run.Playbook != f.Playbook {
			continue
		}
		if hostRuns != nil && !hostRuns[run.ID] {
			continue
		}
		if !f.From.IsZero() && run.StartedAt.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && !run.StartedAt.Before(f.To) {
			continue
		}
		out = append(out, run)
	}
	return out, nil
}

// runSummary is a run record without the full captured output. Listings poll
// every few seconds; returning 50 playbook logs on each poll wastes bandwidth
// and memory. The detail endpoint is the only place that returns output.
func runSummary(run store.RunRecord) store.RunRecord {
	run.Output = ""
	return run
}

// collectionStats returns aggregate stats for one collection.
func (s *Server) collectionStats(ctx context.Context, collection string) (*store.Stats, error) {
	return s.statsForTargets(ctx, map[string]bool{collection: true})
}

// statsForTargets computes aggregate stats over the given collections. A nil
// set means all collections and uses the store's native aggregate query.
func (s *Server) statsForTargets(ctx context.Context, allowed map[string]bool) (*store.Stats, error) {
	if allowed == nil {
		return s.store.Stats(ctx)
	}
	runs, err := s.store.ListRuns(ctx, allRuns)
	if err != nil {
		return nil, err
	}
	hosts, err := s.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	st := &store.Stats{PerPlaybook: []store.PlaybookStats{}}
	var totalDuration int64
	for _, run := range runs {
		if !allowed[run.Target] {
			continue
		}
		st.Runs.Total++
		if run.Success {
			st.Runs.Succeeded++
		} else {
			st.Runs.Failed++
		}
		totalDuration += run.DurationMS
		if run.FinishedAt.After(st.Runs.LastRunAt) {
			st.Runs.LastRunAt = run.FinishedAt
		}
	}
	if st.Runs.Total > 0 {
		st.Runs.AvgDurationMS = totalDuration / int64(st.Runs.Total)
	}
	for _, h := range hosts {
		if !allowed[h.Target] {
			continue
		}
		st.Hosts.Total++
		if h.InInventory {
			st.Hosts.InInventory++
		}
		if h.Active {
			st.Hosts.Active++
		}
		if h.InInventory && !h.Active {
			st.Hosts.Disappeared++
		}
	}
	type key struct{ target, playbook string }
	byBook := map[key]*store.PlaybookStats{}
	for _, run := range runs {
		if !allowed[run.Target] {
			continue
		}
		k := key{run.Target, run.Playbook}
		pb, ok := byBook[k]
		if !ok {
			pb = &store.PlaybookStats{Target: run.Target, Playbook: run.Playbook}
			byBook[k] = pb
		}
		pb.Total++
		if !run.Success {
			pb.Failed++
		}
		if run.FinishedAt.After(pb.LastRunAt) {
			pb.LastRunAt = run.FinishedAt
		}
	}
	for _, pb := range byBook {
		st.PerPlaybook = append(st.PerPlaybook, *pb)
	}
	sort.Slice(st.PerPlaybook, func(i, j int) bool {
		if st.PerPlaybook[i].Target == st.PerPlaybook[j].Target {
			return st.PerPlaybook[i].Playbook < st.PerPlaybook[j].Playbook
		}
		return st.PerPlaybook[i].Target < st.PerPlaybook[j].Target
	})
	return st, nil
}

// organizations returns all organizations in name order, each with its
// collections. It is derived from the live configuration, so a git sync that
// changes the target list is reflected immediately.
func (s *Server) organizations() []orgInfo {
	byOrg := map[string][]collectionInfo{}
	if s.applier != nil {
		for _, name := range s.applier.TargetNames() {
			t, ok := s.applier.Target(name)
			if !ok {
				continue
			}
			org := orgOf(t)
			pbs := make([]playbookInfo, 0, len(t.Playbooks))
			for _, pb := range t.Playbooks {
				pbs = append(pbs, playbookInfo{
					Name:      pb.Name,
					Playbook:  strings.TrimPrefix(pb.Playbook, "./"),
					Inventory: strings.TrimPrefix(pb.Inventory, "./"),
				})
			}
			byOrg[org] = append(byOrg[org], collectionInfo{
				Name:      name,
				Org:       org,
				Playbooks: pbs,
			})
		}
	}
	names := make([]string, 0, len(byOrg))
	for name := range byOrg {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]orgInfo, 0, len(names))
	for _, name := range names {
		colls := byOrg[name]
		sort.Slice(colls, func(i, j int) bool { return colls[i].Name < colls[j].Name })
		out = append(out, orgInfo{Name: name, Collections: colls})
	}
	return out
}

// visibleOrganizations filters the configured orgs to the collections the
// caller is allowed to see. When restrictions are off it returns everything.
func (s *Server) visibleOrganizations(r *http.Request) []orgInfo {
	all := s.organizations()
	allowed := s.accessibleTargets(r.Context(), r)
	if allowed == nil {
		return all
	}
	out := make([]orgInfo, 0, len(all))
	for _, o := range all {
		colls := make([]collectionInfo, 0, len(o.Collections))
		for _, c := range o.Collections {
			if allowed[c.Name] {
				colls = append(colls, c)
			}
		}
		if len(colls) > 0 {
			o.Collections = colls
			out = append(out, o)
		}
	}
	return out
}

// collectionIn resolves a collection by org and name. Collections are
// configured targets; an org is the group named in collection.yml (or
// "default" when omitted).
func (s *Server) collectionIn(org, name string) (config.TargetConfig, bool) {
	if s.applier == nil {
		return config.TargetConfig{}, false
	}
	t, ok := s.applier.Target(name)
	if !ok || orgOf(t) != org {
		return config.TargetConfig{}, false
	}
	return t, true
}

// collectionScope applies the org/collection path parameters of a
// collection-level route. It returns the collection name to filter on, or
// ok=false when the org/collection pair does not exist. On flat routes it
// returns an empty name and ok=true (no scoping).
func (s *Server) collectionScope(r *http.Request) (string, bool) {
	name := r.PathValue("collection")
	if name == "" {
		return "", true
	}
	if _, ok := s.collectionIn(r.PathValue("org"), name); !ok {
		return "", false
	}
	return name, true
}

// requireCollection resolves the org/collection of a route that must have
// both path parameters, answering 404 when either is unknown.
func (s *Server) requireCollection(w http.ResponseWriter, r *http.Request) (config.TargetConfig, bool) {
	t, ok := s.collectionIn(r.PathValue("org"), r.PathValue("collection"))
	if !ok {
		writeError(w, http.StatusNotFound, unknownCollection(r.PathValue("org"), r.PathValue("collection")))
	}
	return t, ok
}

// registerOrgRoutes adds the first-class org and collection resources. These
// exist only in the versioned API: there is no flat alias for an org.
func (s *Server) registerOrgRoutes(mux *http.ServeMux) {
	// GET /api/v1/orgs lists every organization and its collections.
	mux.HandleFunc("GET "+apiPrefix+"/orgs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.visibleOrganizations(r))
	})

	// GET /api/v1/orgs/{org} returns one organization.
	mux.HandleFunc("GET "+apiPrefix+"/orgs/{org}", func(w http.ResponseWriter, r *http.Request) {
		org := r.PathValue("org")
		for _, o := range s.visibleOrganizations(r) {
			if o.Name == org {
				writeJSON(w, http.StatusOK, o)
				return
			}
		}
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown org %q", org))
	})

	// GET /api/v1/orgs/{org}/collections lists the collections of an org.
	mux.HandleFunc("GET "+apiPrefix+"/orgs/{org}/collections", func(w http.ResponseWriter, r *http.Request) {
		org := r.PathValue("org")
		for _, o := range s.visibleOrganizations(r) {
			if o.Name == org {
				writeJSON(w, http.StatusOK, o.Collections)
				return
			}
		}
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown org %q", org))
	})

	// GET /api/v1/orgs/{org}/collections/{collection} describes one
	// collection: its playbooks and the sub-resource paths available for it.
	mux.HandleFunc("GET "+apiPrefix+"/orgs/{org}/collections/{collection}", func(w http.ResponseWriter, r *http.Request) {
		t, ok := s.requireCollection(w, r)
		if !ok {
			return
		}
		if !s.mayUse(r.Context(), r, r.PathValue("org"), t.Name) {
			writeError(w, http.StatusForbidden, errors.New("access to this collection is denied"))
			return
		}
		pbs := make([]playbookInfo, 0, len(t.Playbooks))
		for _, pb := range t.Playbooks {
			pbs = append(pbs, playbookInfo{
				Name:      pb.Name,
				Playbook:  strings.TrimPrefix(pb.Playbook, "./"),
				Inventory: strings.TrimPrefix(pb.Inventory, "./"),
			})
		}
		base := apiPrefix + "/orgs/" + t.Org + "/collections/" + t.Name
		if t.Org == "" {
			base = apiPrefix + "/orgs/" + config.DefaultOrg + "/collections/" + t.Name
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name":      t.Name,
			"org":       orgOf(t),
			"playbooks": pbs,
			"links": map[string]string{
				"runs":      base + "/runs",
				"hosts":     base + "/hosts",
				"stats":     base + "/stats",
				"playbooks": base + "/playbooks",
				"run":       base + "/run",
				"sync":      base + "/sync",
			},
		})
	})
}

// apiAliasPath maps a versioned request path to its legacy flat alias. It is
// used by auth to apply the same rules (login endpoints, sync token, ...)
// whatever prefix a client used.
func apiAliasPath(path string) string {
	if !strings.HasPrefix(path, apiPrefix) {
		return path
	}
	return "/api" + strings.TrimPrefix(path, apiPrefix)
}

// unknownCollection is the error for an invalid org/collection pair.
func unknownCollection(org, collection string) error {
	return fmt.Errorf("unknown collection %q in org %q", collection, org)
}
