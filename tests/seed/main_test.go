package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kakwa/strangle/auth"
	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/store"
)

type datasetCounts struct {
	orgs        int
	collections int
	hosts       int
	runs        int
	failed      int
	// grants maps username -> org -> number of collections granted.
	grants    map[string]map[string]int
	passwords map[string]bool
}

func openAndList(ctx context.Context, dsn string) (datasetCounts, error) {
	var out datasetCounts
	d, err := store.Open(ctx, dsn)
	if err != nil {
		return out, err
	}
	defer d.Close()
	orgs, err := d.ListOrgs(ctx)
	if err != nil {
		return out, err
	}
	hosts, err := d.ListHosts(ctx)
	if err != nil {
		return out, err
	}
	runs, err := d.ListRuns(ctx, 0)
	if err != nil {
		return out, err
	}
	targets := map[string]struct{}{}
	for _, h := range hosts {
		targets[h.Target] = struct{}{}
	}
	for _, r := range runs {
		if !r.Success {
			out.failed++
		}
	}
	out.orgs, out.collections, out.hosts, out.runs = len(orgs), len(targets), len(hosts), len(runs)

	users, err := d.Users().ListUsers(ctx)
	if err != nil {
		return out, err
	}
	out.grants, out.passwords = map[string]map[string]int{}, map[string]bool{}
	for _, u := range users {
		out.grants[u.Name] = map[string]int{}
		out.passwords[u.Name] = auth.VerifyPassword(u.Hash, u.Name)
	}
	memberships, err := d.Access().List(ctx, "")
	if err != nil {
		return out, err
	}
	for _, m := range memberships {
		out.grants[m.Username][m.Org]++
	}
	return out, nil
}

func testOptions() options {
	return options{
		DSN: "unused", Seed: 1, Orgs: 3,
		CollectionsMin: 5, CollectionsMax: 10,
		PlaybooksMin: 1, PlaybooksMax: 5,
		HostsMin: 5, HostsMax: 1000,
		RunsMin: 2, RunsMax: 5,
		FailureRate: 0.25,
	}
}

func TestPlanShapeAndDeterminism(t *testing.T) {
	o := testOptions()
	if err := o.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	specs := plan(o)
	if got := len(specs); got < 3*o.CollectionsMin || got > 3*o.CollectionsMax {
		t.Fatalf("collections = %d, want between %d and %d", got, 3*o.CollectionsMin, 3*o.CollectionsMax)
	}

	byOrg := map[string]int{}
	for _, coll := range specs {
		byOrg[coll.Org]++
		if len(coll.Playbooks) < o.PlaybooksMin || len(coll.Playbooks) > o.PlaybooksMax {
			t.Errorf("%s playbooks = %d, want [%d,%d]", coll.Name, len(coll.Playbooks), o.PlaybooksMin, o.PlaybooksMax)
		}
		if len(coll.Hosts) < o.HostsMin || len(coll.Hosts) > o.HostsMax {
			t.Errorf("%s hosts = %d, want [%d,%d]", coll.Name, len(coll.Hosts), o.HostsMin, o.HostsMax)
		}
	}
	if len(byOrg) != o.Orgs {
		t.Fatalf("orgs = %d, want %d (%v)", len(byOrg), o.Orgs, byOrg)
	}
	for org, n := range byOrg {
		if n < o.CollectionsMin || n > o.CollectionsMax {
			t.Errorf("org %s collections = %d, want [%d,%d]", org, n, o.CollectionsMin, o.CollectionsMax)
		}
	}
	if got := len(specs[0].Hosts); got != o.HostsMin {
		t.Errorf("smallest collection hosts = %d, want %d", got, o.HostsMin)
	}
	if got := len(specs[len(specs)-1].Hosts); got != o.HostsMax {
		t.Errorf("largest collection hosts = %d, want %d", got, o.HostsMax)
	}
	if again := plan(o); !reflect.DeepEqual(specs, again) {
		t.Error("plan is not reproducible with the same seed")
	}
	o2 := o
	o2.Seed++
	if other := plan(o2); reflect.DeepEqual(specs, other) {
		t.Error("different seeds produced the same plan")
	}
}

func TestSeedEmitsLoadableConfig(t *testing.T) {
	ctx := context.Background()
	o := testOptions()
	o.DSN = filepath.Join(t.TempDir(), "seed.db")
	o.ConfigDir = filepath.Join(t.TempDir(), "config")
	o.Orgs = 2
	o.CollectionsMin, o.CollectionsMax = 2, 2
	o.PlaybooksMin, o.PlaybooksMax = 2, 2
	o.HostsMin, o.HostsMax = 5, 5
	o.RunsMin, o.RunsMax = 1, 1

	if err := seed(ctx, o); err != nil {
		t.Fatalf("seed: %v", err)
	}
	conf, err := cfg.Load(filepath.Join(o.ConfigDir, "server.yml"), filepath.Join(o.ConfigDir, "collection.yml"))
	if err != nil {
		t.Fatalf("load generated config: %v", err)
	}
	if conf.Server.Database.DSN != o.DSN {
		t.Errorf("server DSN = %q, want %q", conf.Server.Database.DSN, o.DSN)
	}
	if conf.Server.Schedule != "@every 87600h" {
		t.Errorf("server schedule = %q, want the no-op schedule", conf.Server.Schedule)
	}

	specs := plan(o)
	if len(conf.Targets) != len(specs) {
		t.Fatalf("targets = %d, want %d", len(conf.Targets), len(specs))
	}
	byName := make(map[string]collectionSpec, len(specs))
	for _, s := range specs {
		byName[s.Name] = s
	}
	for _, got := range conf.Targets {
		want, ok := byName[got.Name]
		if !ok {
			t.Errorf("unexpected target %q", got.Name)
			continue
		}
		if got.Org != want.Org {
			t.Errorf("target %q org = %q, want %q", got.Name, got.Org, want.Org)
		}
		if len(got.Playbooks) != len(want.Playbooks) {
			t.Errorf("target %q playbooks = %d, want %d", got.Name, len(got.Playbooks), len(want.Playbooks))
			continue
		}
		for i, pb := range got.Playbooks {
			if pb.Name != want.Playbooks[i].Name {
				t.Errorf("target %q playbook %d = %q, want %q", got.Name, i, pb.Name, want.Playbooks[i].Name)
			}
			if pb.Schedule != "@every 87600h" {
				t.Errorf("target %q playbook %q schedule = %q, want the no-op schedule", got.Name, pb.Name, pb.Schedule)
			}
		}
		for _, pb := range got.Playbooks {
			if _, err := os.Stat(filepath.Join(got.Path, pb.Playbook)); err != nil {
				t.Errorf("generated playbook %s/%s: %v", got.Name, pb.Playbook, err)
			}
		}
	}
}

func TestSeedPopulatesStore(t *testing.T) {
	ctx := context.Background()
	o := testOptions()
	o.DSN = filepath.Join(t.TempDir(), "seed.db")
	o.Orgs = 3
	o.CollectionsMin, o.CollectionsMax = 5, 5
	o.PlaybooksMin, o.PlaybooksMax = 1, 1
	o.HostsMin, o.HostsMax = 5, 5
	o.RunsMin, o.RunsMax = 2, 2
	o.FailureRate = 0 // only the forced per-org failures should fail

	if err := seed(ctx, o); err != nil {
		t.Fatalf("seed: %v", err)
	}

	orgs, err := openAndList(ctx, o.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if orgs.orgs != o.Orgs || orgs.collections != 15 || orgs.hosts != 75 {
		t.Errorf("dataset = %+v, want %d orgs, 15 collections, 75 hosts", orgs, o.Orgs)
	}
	if orgs.runs != 30 || orgs.failed < 3 {
		t.Errorf("dataset = %+v, want 30 runs with at least 3 failures", orgs)
	}
	if orgs.failed >= orgs.runs {
		t.Errorf("dataset = %+v, want some successful runs too", orgs)
	}

	if len(orgs.grants) != 3 {
		t.Fatalf("users = %d, want 3 (%v)", len(orgs.grants), orgs.grants)
	}
	for _, name := range []string{"admin", "one-org", "two-orgs"} {
		if !orgs.passwords[name] {
			t.Errorf("user %q password is not its login", name)
		}
	}
	checkGrants := func(user string, want map[string]int) {
		t.Helper()
		if got := orgs.grants[user]; !reflect.DeepEqual(got, want) {
			t.Errorf("user %q grants = %v, want %v", user, got, want)
		}
	}
	checkGrants("admin", map[string]int{"default": 5, "lab": 5, "ops": 5})
	checkGrants("one-org", map[string]int{"default": 5})
	checkGrants("two-orgs", map[string]int{"default": 5, "lab": 5})
}
