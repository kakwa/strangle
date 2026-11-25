package db_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kakwa/strangle/internal/cfg"
	"github.com/kakwa/strangle/internal/db"
	"github.com/kakwa/strangle/internal/store"
	"github.com/kakwa/strangle/internal/testutil"
)

// TestOrgCollectionSchema checks that collection.yml is mirrored into the
// org/collection tables, that a reload overwrites the previous content and
// that the org/user/group many-to-many tables are migrated.
func TestOrgCollectionSchema(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.TestDSN(t, "schema")
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	conn := testutil.SQLDB(t, dsn)

	targets := []cfg.TargetConfig{{
		Name: "home", Org: cfg.DefaultOrg, Path: "/tmp/home", Schedule: "@every 1m",
		Playbooks: []cfg.PlaybookConfig{{Name: "site", Options: cfg.PlaybookOptions{User: "root"}}},
	}}
	if err := d.SyncCollections(ctx, targets); err != nil {
		t.Fatalf("SyncCollections: %v", err)
	}

	var path, options, playbooks, settings string
	if err := conn.QueryRow(`SELECT path, options::text, playbooks::text, settings::text
		FROM collections WHERE name = 'home'`).Scan(&path, &options, &playbooks, &settings); err != nil {
		t.Fatalf("query collection: %v", err)
	}
	if path != "/tmp/home" {
		t.Errorf("collection path = %q, want /tmp/home", path)
	}
	assertContains(t, options, `"User":"root"`)
	assertContains(t, playbooks, `"Name":"site"`)
	assertContains(t, settings, `"path":"/tmp/home"`)

	// A second load overwrites the row rather than appending to it.
	targets[0].Path = "/tmp/other"
	targets[0].Playbooks = nil
	if err := d.SyncCollections(ctx, targets); err != nil {
		t.Fatalf("second SyncCollections: %v", err)
	}
	if err := conn.QueryRow(`SELECT path, playbooks::text FROM collections WHERE name = 'home'`).
		Scan(&path, &playbooks); err != nil {
		t.Fatalf("query reloaded collection: %v", err)
	}
	if path != "/tmp/other" || playbooks != "[]" {
		t.Errorf("reloaded collection = (%q, %s), want (/tmp/other, [])", path, playbooks)
	}

	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM orgs WHERE name = 'default'`).Scan(&count); err != nil || count != 1 {
		t.Errorf("default org count = %d, %v; want 1", count, err)
	}
	for _, table := range []string{"org_users", "org_groups", "group_users"} {
		if err := conn.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
			t.Errorf("many-to-many table %s is missing: %v", table, err)
		}
	}

	// Removing a collection from the file removes its database row.
	if err := d.SyncCollections(ctx, []cfg.TargetConfig{{Name: "new", Path: "/tmp/new"}}); err != nil {
		t.Fatalf("third SyncCollections: %v", err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM collections WHERE name = 'home'`).Scan(&count); err != nil || count != 0 {
		t.Errorf("stale collection count = %d, %v; want 0", count, err)
	}
}

// TestHostAndAnsibleRunSchema checks the context key of hosts, the jsonb
// facts column and the per-host run rows with jsonb tasks and stats.
func TestHostAndAnsibleRunSchema(t *testing.T) {
	ctx := context.Background()
	dsn := testutil.TestDSN(t, "schema-runs")
	d, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()
	conn := testutil.SQLDB(t, dsn)

	if err := d.SyncCollections(ctx, []cfg.TargetConfig{{Name: "home", Path: "/tmp/home"}}); err != nil {
		t.Fatalf("SyncCollections: %v", err)
	}

	save := func(playbook string, finished time.Time) {
		t.Helper()
		err := d.SaveRun(ctx, store.RunInput{
			Run: store.RunRecord{
				Target: "home", Playbook: playbook, Inventory: "hosts.ini",
				Check: true, StartedAt: finished.Add(-time.Minute), FinishedAt: finished,
			},
			Hosts: []store.HostRunRecord{{Host: "web1", Status: "changed", Ok: 2, Changed: 1}},
			Tasks: []store.TaskStateRecord{{Host: "web1", Task: "install nginx", Status: "changed"}},
			Facts: map[string]json.RawMessage{"web1": json.RawMessage(`{"os":"linux"}`)},
		})
		if err != nil {
			t.Fatalf("SaveRun(%s): %v", playbook, err)
		}
	}
	now := time.Now().UTC()
	save("site.yml", now)
	save("other.yml", now.Add(time.Hour))

	var contexts, linked, runs int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM hosts WHERE name = 'web1'`).Scan(&contexts); err != nil || contexts != 2 {
		t.Errorf("host contexts = %d, %v; want 2", contexts, err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM hosts h
		JOIN collections c ON c.id = h.collection_id
		JOIN orgs o ON o.id = h.org_id
		WHERE h.name = 'web1' AND c.name = 'home' AND o.name = 'default'`).Scan(&linked); err != nil || linked != 2 {
		t.Errorf("linked hosts = %d, %v; want 2", linked, err)
	}
	if err := conn.QueryRow(`SELECT COUNT(*) FROM ansible_runs ar
		JOIN hosts h ON h.id = ar.host_id
		WHERE h.name = 'web1' AND ar.mode = 'check'`).Scan(&runs); err != nil || runs != 2 {
		t.Errorf("ansible runs = %d, %v; want 2", runs, err)
	}

	var tasks, stats, facts string
	if err := conn.QueryRow(`SELECT tasks::text, stats::text, h.facts::text FROM ansible_runs ar
		JOIN hosts h ON h.id = ar.host_id
		WHERE h.playbook = 'site.yml' LIMIT 1`).Scan(&tasks, &stats, &facts); err != nil {
		t.Fatalf("query ansible run: %v", err)
	}
	assertContains(t, tasks, `"task":"install nginx"`)
	assertContains(t, stats, `"changed":1`)
	assertContains(t, stats, `"mode":"check"`)
	testutil.AssertJSONEqual(t, json.RawMessage(facts), json.RawMessage(`{"os":"linux"}`))
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	// PostgreSQL's jsonb output inserts spaces after colons and commas.
	compact := strings.NewReplacer(": ", ":", ", ", ",").Replace(got)
	if !strings.Contains(compact, want) {
		t.Errorf("json %q does not contain %q", got, want)
	}
}
