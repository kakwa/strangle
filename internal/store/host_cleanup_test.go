package store

import (
	"context"
	"testing"
	"time"
)

func seedHostRun(t *testing.T, st *Mem, id uint64, target string, finished time.Time, inventory []InventoryHost, hosts []string) {
	t.Helper()
	run := testRun(target, "site", finished)
	run.ID = id
	run.State = RunStateCompleted

	recaps := make([]HostRunRecord, 0, len(hosts))
	tasks := make([]TaskStateRecord, 0, len(hosts))
	for _, name := range hosts {
		recaps = append(recaps, HostRunRecord{Host: name, Ok: 1})
		tasks = append(tasks, TaskStateRecord{Host: name, Task: "install", Status: "ok", LastRunID: id})
	}
	if err := st.SaveRun(context.Background(), RunInput{
		Run: run, Hosts: recaps, Tasks: tasks, Inventory: inventory,
	}); err != nil {
		t.Fatalf("SaveRun(%d) error: %v", id, err)
	}
}

func TestMemPruneHosts(t *testing.T) {
	st := NewMem()
	defer st.Close()
	ctx := context.Background()
	now := time.Now().UTC()

	seedHostRun(t, st, 1, "home", now.Add(-72*time.Hour),
		[]InventoryHost{{Name: "old"}, {Name: "recent"}}, []string{"old", "recent"})
	seedHostRun(t, st, 2, "home", now.Add(-time.Hour),
		[]InventoryHost{{Name: "recent"}}, []string{"recent"})
	seedHostRun(t, st, 3, "edge", now.Add(-72*time.Hour),
		[]InventoryHost{{Name: "old"}}, []string{"old"})
	// The PostgreSQL implementation clears this when a later inventory
	// snapshot drops the host; do it directly here (the in-memory store only
	// sets the flag for hosts present in a snapshot).
	st.hosts["home\x00old"].InInventory = false

	removed, err := st.PruneHosts(ctx, "home", 24*time.Hour)
	if err != nil {
		t.Fatalf("PruneHosts() error: %v", err)
	}
	if removed != 1 {
		t.Fatalf("PruneHosts() = %d, want 1", removed)
	}

	hosts, err := st.ListHosts(ctx)
	if err != nil {
		t.Fatalf("ListHosts() error: %v", err)
	}
	if len(hosts) != 2 {
		t.Fatalf("remaining hosts = %v, want recent (home) and old (edge)", hosts)
	}
	for _, h := range hosts {
		if h.Target == "home" && h.Name == "old" {
			t.Error("stale home host was not removed")
		}
		if h.Target == "edge" && h.Name == "old" {
			// Same name, different target: kept.
			continue
		}
	}

	// The removed host's per-host recaps and task history are gone, but the
	// edge host with the same name keeps its own history.
	hostRuns, err := st.HostRuns(ctx, "old", 10)
	if err != nil {
		t.Fatalf("HostRuns() error: %v", err)
	}
	if len(hostRuns) != 1 || hostRuns[0].Target != "edge" {
		t.Fatalf("HostRuns(old) = %v, want only the edge run", hostRuns)
	}
	changes, err := st.HostChanges(ctx, "old", 10)
	if err != nil {
		t.Fatalf("HostChanges() error: %v", err)
	}
	if len(changes) != 1 || changes[0].Target != "edge" {
		t.Fatalf("HostChanges(old) = %v, want only the edge task", changes)
	}

	// A disabled policy never deletes anything.
	before, _ := st.ListHosts(ctx)
	removed, err = st.PruneHosts(ctx, "home", 0)
	if err != nil || removed != 0 {
		t.Fatalf("disabled PruneHosts() = %d, %v", removed, err)
	}
	after, _ := st.ListHosts(ctx)
	if len(after) != len(before) {
		t.Errorf("disabled policy removed hosts: %v", after)
	}
}
