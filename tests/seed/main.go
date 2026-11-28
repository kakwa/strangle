// Command strangle-seed fills a fresh strangle database with a deterministic
// test dataset: organizations, collections, playbooks, hosts and past playbook
// runs (a share of them failed). It also creates three UI accounts (password
// equal to the login) with grants for all, one and two orgs. It goes through
// the real store APIs, so the generated rows are exactly what the server, API
// and UI read.
//
// Examples:
//
//	go run ./tests/seed -dsn ./strangle-seed.db -reset
//	go run ./tests/seed -dsn 'postgres://user:pass@localhost/strangle_seed' -config-dir ./tests/seed/generated
//
// The target database must be empty (apart from the default org created by
// store.Open). -reset deletes a SQLite file first; a PostgreSQL database has
// to be created empty. -config-dir writes the matching server.yml,
// collection.yml and per-target strangle.yml files so the server can be
// started with the same orgs/collections/playbooks the seed uses. See
// README.md in this directory for the full procedure.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kakwa/strangle/auth"
	"github.com/kakwa/strangle/cfg"
	"github.com/kakwa/strangle/store"
)

type options struct {
	DSN            string
	ConfigDir      string
	Reset          bool
	Seed           int64
	Orgs           int
	CollectionsMin int
	CollectionsMax int
	PlaybooksMin   int
	PlaybooksMax   int
	HostsMin       int
	HostsMax       int
	RunsMin        int
	RunsMax        int
	FailureRate    float64
}

func main() {
	var o options
	flag.StringVar(&o.DSN, "dsn", "strangle-seed.db", "SQLite file path or Postgres DSN to seed")
	flag.StringVar(&o.ConfigDir, "config-dir", "", "write matching server.yml/collection.yml/strangle.yml files here (needed to run the server)")
	flag.BoolVar(&o.Reset, "reset", false, "delete an existing SQLite file before seeding (not supported for Postgres)")
	flag.Int64Var(&o.Seed, "seed", 1, "random seed; the same seed reproduces the same dataset")
	flag.IntVar(&o.Orgs, "orgs", 3, "number of organizations (includes the default org)")
	flag.IntVar(&o.CollectionsMin, "collections-min", 5, "minimum collections per org")
	flag.IntVar(&o.CollectionsMax, "collections-max", 10, "maximum collections per org")
	flag.IntVar(&o.PlaybooksMin, "playbooks-min", 1, "minimum playbooks per collection")
	flag.IntVar(&o.PlaybooksMax, "playbooks-max", 5, "maximum playbooks per collection")
	flag.IntVar(&o.HostsMin, "hosts-min", 5, "minimum hosts per collection")
	flag.IntVar(&o.HostsMax, "hosts-max", 1000, "maximum hosts per collection")
	flag.IntVar(&o.RunsMin, "runs-min", 2, "minimum past runs per playbook")
	flag.IntVar(&o.RunsMax, "runs-max", 5, "maximum past runs per playbook")
	flag.Float64Var(&o.FailureRate, "failure-rate", 0.25, "probability that a past run fails (0-1)")
	flag.Parse()

	if err := seed(context.Background(), o); err != nil {
		log.Fatal(err)
	}
}

func (o options) validate() error {
	for name, pair := range map[string][2]int{
		"collections": {o.CollectionsMin, o.CollectionsMax},
		"playbooks":   {o.PlaybooksMin, o.PlaybooksMax},
		"hosts":       {o.HostsMin, o.HostsMax},
		"runs":        {o.RunsMin, o.RunsMax},
	} {
		if pair[0] < 1 || pair[0] > pair[1] {
			return fmt.Errorf("%s range invalid: min=%d max=%d", name, pair[0], pair[1])
		}
	}
	if o.Orgs < 1 {
		return errors.New("orgs must be at least 1")
	}
	if o.FailureRate < 0 || o.FailureRate > 1 {
		return fmt.Errorf("failure-rate must be between 0 and 1, got %v", o.FailureRate)
	}
	if strings.TrimSpace(o.DSN) == "" {
		return errors.New("dsn is required")
	}
	if o.Reset {
		if isPostgres(o.DSN) {
			return errors.New("-reset only supports SQLite files; create an empty PostgreSQL database instead")
		}
		if _, ok := sqlitePath(o.DSN); !ok {
			return fmt.Errorf("-reset cannot resolve a file path from DSN %q", o.DSN)
		}
	}
	return nil
}

// collectionSpec is the plan for one collection: its config plus the host
// names that appear in its playbook recaps.
type collectionSpec struct {
	Org        string
	Name       string
	Path       string
	WorkingDir string
	Playbooks  []playbookSpec
	Hosts      []string
}

type playbookSpec struct {
	Name      string
	File      string
	Inventory string
}

var playbookNames = []string{"web", "database", "base", "hardening", "observability"}

func playbookName(i int) string {
	if i < len(playbookNames) {
		return playbookNames[i]
	}
	return fmt.Sprintf("playbook-%d", i+1)
}

// orgNames returns the deterministic org names; the first one is always the
// default org that store.Open creates.
func orgNames(count int) []string {
	names := make([]string, count)
	for i := range names {
		switch i {
		case 0:
			names[i] = cfg.DefaultOrg
		case 1:
			names[i] = "lab"
		case 2:
			names[i] = "ops"
		default:
			names[i] = fmt.Sprintf("org-%d", i+1)
		}
	}
	return names
}

// seedUser is a local UI account. Its password is always its own login, which
// keeps the test credentials easy to remember. Orgs lists the organizations
// whose collections the account may use; grants are stored as explicit
// collection memberships because that is what the API checks.
type seedUser struct {
	Name string
	Orgs []string
}

// usersFor builds the three accounts: access to every org, to one org and to
// two orgs. With fewer than two orgs configured, "two-orgs" covers all of
// them; with more, it covers the first two.
func usersFor(orgs []string) []seedUser {
	two := orgs
	if len(two) > 2 {
		two = two[:2]
	}
	return []seedUser{
		{Name: "admin", Orgs: orgs},
		{Name: "one-org", Orgs: orgs[:1]},
		{Name: "two-orgs", Orgs: two},
	}
}

func inOrganizations(orgs []string, org string) bool {
	for _, o := range orgs {
		if o == org {
			return true
		}
	}
	return false
}

// plan builds the whole dataset shape without touching the database. It is
// deterministic for a given seed, which makes the generator reproducible and
// its shape easy to test.
func plan(o options) []collectionSpec {
	//nolint:gosec // deterministic test data, not security-sensitive
	rng := rand.New(rand.NewSource(o.Seed))
	var specs []collectionSpec
	for _, org := range orgNames(o.Orgs) {
		n := between(rng, o.CollectionsMin, o.CollectionsMax)
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("%s-c%02d", org, i+1)
			pbCount := between(rng, o.PlaybooksMin, o.PlaybooksMax)
			playbooks := make([]playbookSpec, 0, pbCount)
			for j := 0; j < pbCount; j++ {
				pbName := playbookName(j)
				playbooks = append(playbooks, playbookSpec{
					Name:      pbName,
					File:      "playbooks/" + pbName + ".yml",
					Inventory: "inventory/" + name + ".ini",
				})
			}
			specs = append(specs, collectionSpec{
				Org:        org,
				Name:       name,
				Path:       "/srv/ansible/" + org + "/" + name,
				WorkingDir: "/srv/ansible/" + org + "/" + name,
				Playbooks:  playbooks,
				Hosts:      makeHosts(name, hostCount(rng, o.HostsMin, o.HostsMax)),
			})
		}
	}
	// Keep both ends of the requested range present in every generated
	// dataset, so load tests always get at least one tiny and one large
	// collection.
	specs[0].Hosts = makeHosts(specs[0].Name, o.HostsMin)
	last := specs[len(specs)-1]
	last.Hosts = makeHosts(last.Name, o.HostsMax)
	specs[len(specs)-1] = last
	return specs
}

func makeHosts(collection string, count int) []string {
	hosts := make([]string, count)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("%s-h%04d", collection, i+1)
	}
	return hosts
}

// hostCount biases towards smaller collections but can reach any value in
// [min, max], including the extremes.
func hostCount(rng *rand.Rand, min, max int) int {
	span := max - min
	if span <= 0 {
		return min
	}
	var lo, hi int
	switch roll := rng.Float64(); {
	case roll < 0.4:
		lo, hi = 0, 1
	case roll < 0.7:
		lo, hi = 1, 4
	case roll < 0.9:
		lo, hi = 4, 7
	default:
		lo, hi = 7, 10
	}
	low := min + span*lo/10
	high := min + span*hi/10
	if low > high {
		low = high
	}
	if high > max {
		high = max
	}
	return between(rng, low, high)
}

func between(rng *rand.Rand, lo, hi int) int {
	if hi <= lo {
		return lo
	}
	return lo + rng.Intn(hi-lo+1)
}

func seed(ctx context.Context, o options) error {
	if err := o.validate(); err != nil {
		return err
	}
	if o.Reset {
		path, _ := sqlitePath(o.DSN)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %q: %w", path, err)
		}
	} else if path, ok := sqlitePath(o.DSN); ok {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("create database directory: %w", err)
		}
	}

	d, err := store.Open(ctx, o.DSN)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if err := d.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	if err := requireEmpty(ctx, d); err != nil {
		return err
	}
	specs := plan(o)
	for _, org := range orgNames(o.Orgs)[1:] {
		if _, err := d.CreateOrg(ctx, org); err != nil {
			return fmt.Errorf("create org %q: %w", org, err)
		}
	}

	targets := make([]cfg.TargetConfig, 0, len(specs))
	for _, coll := range specs {
		pbs := make([]cfg.PlaybookConfig, 0, len(coll.Playbooks))
		for _, pb := range coll.Playbooks {
			pbs = append(pbs, cfg.PlaybookConfig{
				Name:       pb.Name,
				TargetName: coll.Name,
				TargetPath: coll.Path,
				Playbook:   pb.File,
				Inventory:  pb.Inventory,
				WorkingDir: coll.WorkingDir,
			})
		}
		targets = append(targets, cfg.TargetConfig{
			Name: coll.Name, Org: coll.Org, Path: coll.Path,
			WorkingDir: coll.WorkingDir, Playbooks: pbs,
		})
	}
	if err := d.SyncCollections(ctx, targets); err != nil {
		return fmt.Errorf("sync collections: %w", err)
	}
	log.Printf("created %d orgs and %d collections", o.Orgs, len(specs))
	if o.ConfigDir != "" {
		if err := writeConfig(o, specs); err != nil {
			return err
		}
		log.Printf("wrote config for %d targets to %s", len(specs), o.ConfigDir)
	}
	if err := seedUsers(ctx, d, orgNames(o.Orgs), specs); err != nil {
		return err
	}
	log.Printf("created 3 users (password = login) with collection grants")

	//nolint:gosec // deterministic test data, not security-sensitive
	rng := rand.New(rand.NewSource(o.Seed + 1))
	now := time.Now().UTC()
	runs, failed, hostRunRows := 0, 0, 0
	for collIdx, coll := range specs {
		for pbIdx, pb := range coll.Playbooks {
			runCount := between(rng, o.RunsMin, o.RunsMax)
			for runIdx := 0; runIdx < runCount; runIdx++ {
				// The newest run of each playbook sees every host, so every
				// generated host has run history. Older runs see a subset.
				allHosts := runIdx == 0
				runHosts := pickHosts(rng, coll.Hosts, allHosts)
				// Force one failing run per org, then apply the random rate.
				fail := collIdx%o.Orgs == 0 && pbIdx == 0 && runIdx == 0 ||
					rng.Float64() < o.FailureRate
				started := now.Add(-time.Duration(runIdx)*24*time.Hour - time.Duration(rng.Intn(12))*time.Hour)
				finished := started.Add(time.Duration(5+rng.Intn(120)) * time.Second)
				if err := saveRun(ctx, d, rng, coll, pb, collIdx, runHosts, fail, started, finished); err != nil {
					return fmt.Errorf("seed run %s/%s: %w", coll.Name, pb.Name, err)
				}
				runs++
				hostRunRows += len(runHosts)
				if fail {
					failed++
				}
			}
		}
	}

	allRuns, err := d.ListRuns(ctx, 0)
	if err != nil {
		return fmt.Errorf("count runs: %w", err)
	}
	allHosts, err := d.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("count hosts: %w", err)
	}
	log.Printf("done: %d runs (%d failed, %d host recap rows), %d distinct hosts; dsn=%s seed=%d",
		len(allRuns), failed, hostRunRows, len(allHosts), o.DSN, o.Seed)
	if runs != len(allRuns) {
		log.Printf("note: %d seeded runs, %d stored runs", runs, len(allRuns))
	}
	return nil
}

// seedUsers creates the UI accounts and their collection grants. Once any
// membership exists, the API restricts every user to explicit grants, so the
// all-orgs account needs a grant for every seeded collection.
func seedUsers(ctx context.Context, d *store.DB, orgs []string, specs []collectionSpec) error {
	for _, u := range usersFor(orgs) {
		hash, err := auth.HashPassword(u.Name) // password == login
		if err != nil {
			return fmt.Errorf("hash password for user %q: %w", u.Name, err)
		}
		if err := d.Users().CreateUser(ctx, u.Name, hash); err != nil {
			return fmt.Errorf("create user %q: %w", u.Name, err)
		}
		for _, coll := range specs {
			if !inOrganizations(u.Orgs, coll.Org) {
				continue
			}
			if err := d.Access().Add(ctx, u.Name, coll.Org, coll.Name); err != nil {
				return fmt.Errorf("grant %q access to %s/%s: %w", u.Name, coll.Org, coll.Name, err)
			}
		}
	}
	return nil
}

func requireEmpty(ctx context.Context, d *store.DB) error {
	users, err := d.Users().Count(ctx)
	if err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if users > 0 {
		return fmt.Errorf("database already has %d users; use a fresh database or -reset with SQLite", users)
	}
	orgs, err := d.ListOrgs(ctx)
	if err != nil {
		return fmt.Errorf("list orgs: %w", err)
	}
	if len(orgs) != 1 || orgs[0].Name != cfg.DefaultOrg {
		return fmt.Errorf("database already has orgs (%d); use a fresh database or -reset with SQLite", len(orgs))
	}
	runs, err := d.ListRuns(ctx, 1)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	if len(runs) > 0 {
		return errors.New("database already has runs; use a fresh database or -reset with SQLite")
	}
	hosts, err := d.ListHosts(ctx)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}
	if len(hosts) > 0 {
		return errors.New("database already has hosts; use a fresh database or -reset with SQLite")
	}
	return nil
}

func pickHosts(rng *rand.Rand, hosts []string, all bool) []string {
	if all {
		return append([]string(nil), hosts...)
	}
	n := len(hosts) / between(rng, 3, 10)
	if n < 1 {
		n = 1
	}
	order := rng.Perm(len(hosts))[:n]
	chosen := make([]string, 0, n)
	for _, i := range order {
		chosen = append(chosen, hosts[i])
	}
	return chosen
}

func saveRun(
	ctx context.Context,
	d *store.DB,
	rng *rand.Rand,
	coll collectionSpec,
	pb playbookSpec,
	collIdx int,
	hosts []string,
	fail bool,
	started, finished time.Time,
) error {
	failedHosts := 0
	if fail {
		failedHosts = len(hosts) / 10
		if failedHosts < 1 {
			failedHosts = 1
		}
	}
	changedHosts := 0
	hostRuns := make([]store.HostRunRecord, 0, len(hosts))
	facts := map[string]json.RawMessage{}
	tasks := make([]store.TaskStateRecord, 0, len(hosts))
	for i, host := range hosts {
		status := "ok"
		counters := store.TaskCounters{Ok: 6 + rng.Intn(6), Skipped: rng.Intn(3)}
		switch {
		case i < failedHosts:
			status = "failed"
			counters.Failed = 1 + rng.Intn(3)
			tasks = append(tasks, store.TaskStateRecord{
				Target: coll.Name, Host: host, Task: "Apply configuration", Status: "failed",
				LastError: "task failed: see playbook output", LastRunAt: finished,
			})
		case rng.Float64() < 0.3:
			status = "changed"
			counters.Changed = 1 + rng.Intn(3)
			changedHosts++
		}
		if status != "failed" && rng.Float64() < 0.25 {
			taskStatus, diff := "ok", ""
			if status == "changed" {
				taskStatus, diff = "changed", "+ configuration updated"
			}
			tasks = append(tasks, store.TaskStateRecord{
				Target: coll.Name, Host: host, Task: "Apply configuration",
				Status: taskStatus, LastDiff: diff, LastRunAt: finished,
			})
		}
		hostRuns = append(hostRuns, store.HostRunRecord{
			Target: coll.Name, Host: host, Status: status, TaskCounters: counters,
		})
		if rng.Float64() < 0.3 {
			family := "Debian"
			if rng.Intn(2) == 0 {
				family = "RedHat"
			}
			ip := fmt.Sprintf("10.%d.%d.%d", 20+collIdx%200, 1+i%250, 1+i%250)
			facts[host] = json.RawMessage(fmt.Sprintf(
				`{"ansible_hostname":%q,"ansible_os_family":%q,"ansible_default_ipv4":{"address":%q}}`,
				host, family, ip))
		}
	}

	exitCode := 0
	if fail {
		exitCode = 2
	}
	run := store.RunRecord{
		Target: coll.Name, Playbook: pb.Name, Inventory: pb.Inventory,
		StartedAt: started, FinishedAt: finished,
		DurationMS: finished.Sub(started).Milliseconds(), ExitCode: exitCode,
		Success: !fail, Hosts: len(hosts), ChangedHosts: changedHosts,
		FailedHosts: failedHosts, Output: buildOutput(hostRuns),
		State: store.RunStateCompleted,
	}
	return d.SaveRun(ctx, store.RunInput{Run: run, Hosts: hostRuns, Tasks: tasks, Facts: facts})
}

func buildOutput(hosts []store.HostRunRecord) string {
	var b strings.Builder
	b.WriteString("PLAY [all] **********************************************************\n\n")
	b.WriteString("TASK [Gathering Facts] ***********************************************\n")
	for i, h := range hosts {
		if i >= 20 {
			break
		}
		fmt.Fprintf(&b, "ok: [%s]\n", h.Host)
	}
	b.WriteString("\nTASK [Apply configuration] *******************************************\n")
	for i, h := range hosts {
		if i >= 20 {
			break
		}
		switch h.Status {
		case "failed":
			fmt.Fprintf(&b, "fatal: [%s]: FAILED! => {\"msg\": \"task failed\"}\n", h.Host)
		case "changed":
			fmt.Fprintf(&b, "changed: [%s]\n", h.Host)
		default:
			fmt.Fprintf(&b, "ok: [%s]\n", h.Host)
		}
	}
	b.WriteString("\nPLAY RECAP ***********************************************************\n")
	for _, h := range hosts {
		fmt.Fprintf(&b, "%s : ok=%d changed=%d unreachable=0 failed=%d skipped=%d rescued=0 ignored=0\n",
			h.Host, h.Ok, h.Changed, h.Failed, h.Skipped)
	}
	return b.String()
}

// writeConfig emits the YAML files the server needs to see the seeded data.
// Names and playbooks mirror the database rows exactly: SyncCollections (run
// at server startup) treats collection.yml as authoritative and would delete
// any seeded collection missing from it.
func writeConfig(o options, specs []collectionSpec) error {
	// Drop targets left by a previous generation, but leave anything else in
	// the directory untouched.
	if err := os.RemoveAll(filepath.Join(o.ConfigDir, "collections")); err != nil {
		return fmt.Errorf("clear generated targets: %w", err)
	}

	var coll strings.Builder
	coll.WriteString("# Generated by `go run ./tests/seed`; do not edit by hand.\n")
	coll.WriteString("# Names mirror the rows seeded into the database.\n")
	coll.WriteString("collections:\n")
	for _, c := range specs {
		fmt.Fprintf(&coll, "  - name: %s\n    org: %s\n    path: collections/%s/%s\n",
			c.Name, c.Org, c.Org, c.Name)
	}
	if err := writeFile(filepath.Join(o.ConfigDir, "collection.yml"), coll.String()); err != nil {
		return err
	}

	var server strings.Builder
	server.WriteString("# Generated by `go run ./tests/seed`; do not edit by hand.\n")
	server.WriteString("listen_addr: \":8080\"\n")
	server.WriteString("log_level: info\n")
	// An empty schedule would fall back to @every 15m and start real runs.
	server.WriteString("# Effectively never run a playbook: keep the seeded history as-is.\n")
	server.WriteString("schedule: \"@every 87600h\"\n")
	server.WriteString("workers: 1\n")
	server.WriteString("database:\n")
	fmt.Fprintf(&server, "  dsn: %s\n", strconv.Quote(o.DSN))
	if err := writeFile(filepath.Join(o.ConfigDir, "server.yml"), server.String()); err != nil {
		return err
	}

	for _, c := range specs {
		dir := filepath.Join(o.ConfigDir, "collections", c.Org, c.Name)
		var target strings.Builder
		target.WriteString("# Generated by `go run ./tests/seed`; do not edit by hand.\n")
		target.WriteString("schedule: \"@every 87600h\"\n")
		target.WriteString("playbooks:\n")
		for _, pb := range c.Playbooks {
			fmt.Fprintf(&target, "  - name: %s\n    playbook: %s\n    inventory: %s\n",
				pb.Name, pb.File, pb.Inventory)
			playbook := "# Generated by `go run ./tests/seed`; no-op playbook.\n" +
				"- hosts: all\n  gather_facts: false\n  tasks:\n" +
				"    - name: seeded test collection\n      ansible.builtin.debug:\n" +
				"        msg: seeded test collection\n"
			if err := writeFile(filepath.Join(dir, pb.File), playbook); err != nil {
				return err
			}
		}
		if err := writeFile(filepath.Join(dir, "strangle.yml"), target.String()); err != nil {
			return err
		}
		var inv strings.Builder
		inv.WriteString("# Generated by `go run ./tests/seed`; do not edit by hand.\n[all]\n")
		for _, h := range c.Hosts {
			fmt.Fprintf(&inv, "%s\n", h)
		}
		if err := writeFile(filepath.Join(dir, "inventory", c.Name+".ini"), inv.String()); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func isPostgres(dsn string) bool {
	s := strings.ToLower(strings.TrimSpace(dsn))
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

func sqlitePath(dsn string) (string, bool) {
	s := strings.TrimSpace(dsn)
	if isPostgres(s) {
		return "", false
	}
	for _, prefix := range []string{"sqlite3://", "sqlite://", "file:"} {
		if strings.HasPrefix(strings.ToLower(s), prefix) {
			s = s[len(prefix):]
			break
		}
	}
	if i := strings.IndexByte(s, '?'); i >= 0 {
		s = s[:i] // drop SQLite URI parameters (e.g. ?mode=rwc)
	}
	if s == "" {
		return "", false
	}
	return s, true
}
