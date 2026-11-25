package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/store"
)

func TestCollectionScopedStats(t *testing.T) {
	ctx := context.Background()
	st := store.NewMem()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	seedRunAt(t, st, "home", "site", "home1", now)
	seedRunAt(t, st, "edge", "site", "edge1", now)
	if err := st.SaveRun(ctx, store.RunInput{
		Run: store.RunRecord{
			Target: "edge", Playbook: "deploy", StartedAt: now.Add(time.Hour),
			FinishedAt: now.Add(time.Hour + time.Second), Success: false, Hosts: 1,
		},
		Hosts: []store.HostRunRecord{{Host: "edge1", Status: "failed", Failed: 1}},
	}); err != nil {
		t.Fatalf("SaveRun() error: %v", err)
	}
	s := newOrgTestServer(&recEnqueuer{status: "queued"}, st)

	statsAt := func(path string) store.Stats {
		t.Helper()
		rec := doGet(t, s, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, body %s", path, rec.Code, rec.Body.String())
		}
		var out store.Stats
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return out
	}

	edge := statsAt("/api/v1/orgs/lab/collections/edge/stats")
	if edge.Runs.Total != 2 || edge.Runs.Succeeded != 1 || edge.Runs.Failed != 1 {
		t.Errorf("edge runs = %+v, want two runs (one failed)", edge.Runs)
	}
	if edge.Hosts.Total != 1 {
		t.Errorf("edge hosts = %+v, want only the edge host", edge.Hosts)
	}
	if len(edge.PerPlaybook) != 2 {
		t.Fatalf("edge per-playbook = %+v, want two entries", edge.PerPlaybook)
	}
	for _, pb := range edge.PerPlaybook {
		if pb.Target != "edge" {
			t.Errorf("per-playbook entry = %+v, want only collection edge", pb)
		}
	}

	home := statsAt("/api/v1/orgs/default/collections/home/stats")
	if home.Runs.Total != 1 || home.Runs.Succeeded != 1 || home.Hosts.Total != 1 {
		t.Errorf("home stats = %+v, want one successful run and one host", home)
	}

	if rec := doGet(t, s, "/api/v1/orgs/lab/collections/home/stats"); rec.Code != http.StatusNotFound {
		t.Errorf("wrong org for home: status = %d, want 404", rec.Code)
	}
}
