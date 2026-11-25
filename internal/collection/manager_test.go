package collection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
	"go.uber.org/zap"

	"github.com/kakwa/strangle/internal/config"
)

var testSig = &object.Signature{
	Name:  "strangle-test",
	Email: "strangle-test@example.com",
	When:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

const (
	mainStrangle = `
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory.ini
`
	featureStrangle = `
playbooks:
  - name: feature
    playbook: feature.yml
    inventory: inventories/{{ .Branch }}.ini
`
)

// writeFixtureFile writes a file into the origin worktree.
func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// newOrigin builds a local origin repository with go-git:
//
//	main:    strangle.yml (playbook "site")
//	feature: strangle.yml (playbook "feature", branch-templated inventory)
//	broken:  a strangle.yml without playbooks
//
// and returns the repository path and the branch heads. HEAD is left on
// main.
func newOrigin(t *testing.T) (string, map[string]string) {
	t.Helper()
	origin := filepath.Join(t.TempDir(), "origin")
	repo, err := gogit.PlainInit(origin, false, gogit.WithDefaultBranch(plumbing.ReferenceName("refs/heads/main")))
	if err != nil {
		t.Fatalf("init origin: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	commit := func(msg string, files map[string]string) string {
		t.Helper()
		for name, content := range files {
			writeFixtureFile(t, origin, name, content)
		}
		for name := range files {
			if _, err := wt.Add(name); err != nil {
				t.Fatalf("add %s: %v", name, err)
			}
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: testSig})
		if err != nil {
			t.Fatalf("commit %s: %v", msg, err)
		}
		return h.String()
	}

	mainHead := commit("main", map[string]string{
		"strangle.yml":  mainStrangle,
		"site.yml":      "placeholder\n",
		"inventory.ini": "placeholder\n",
		"feature.yml":   "placeholder\n",
	})
	if err := repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), plumbing.NewHash(mainHead))); err != nil {
		t.Fatalf("create feature: %v", err)
	}
	if err := repo.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("feature"))); err != nil {
		t.Fatalf("switch HEAD to feature: %v", err)
	}
	featureHead := commit("feature", map[string]string{"strangle.yml": featureStrangle})
	if err := repo.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("switch HEAD back to main: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close origin: %v", err)
	}
	return origin, map[string]string{"main": mainHead, "feature": featureHead}
}

// newFixture builds the origin plus a Manager for a git-backed target "one"
// checked out next to the origin. It returns the manager, the origin path
// and the branch heads.
func newFixture(t *testing.T) (*Manager, string, map[string]string) {
	t.Helper()
	origin, revs := newOrigin(t)
	checkoutDir := filepath.Join(t.TempDir(), "checkout")
	targets := []config.TargetConfig{{
		Name: "one",
		Org:  "lab",
		Path: checkoutDir,
		Git:  config.GitConfig{URL: origin, Branch: "main"},
		Notifications: []config.NotificationConfig{{
			Type:    config.NotificationWebhook,
			URL:     "https://example.com/hook",
			Timeout: time.Second,
		}},
		WorkingDir: checkoutDir,
	}}
	m := New(targets, zap.NewNop())
	return m, origin, revs
}

func TestNewAndTargets(t *testing.T) {
	m := New([]config.TargetConfig{
		{Name: "b", Path: "/b"},
		{Name: "a", Path: "/a", Git: config.GitConfig{URL: "https://example.com/a.git"}},
	}, zap.NewNop())

	if !m.HasGit() {
		t.Error("HasGit() = false, want true")
	}
	got := m.Targets()
	if len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Errorf("Targets() = %+v, want sorted [a b]", got)
	}
	if _, ok := m.Target("b"); !ok {
		t.Error("Target(b) = not found")
	}
	if _, ok := m.Target("nope"); ok {
		t.Error("Target(nope) = found, want not found")
	}

	plain := New([]config.TargetConfig{{Name: "a", Path: "/a"}}, zap.NewNop())
	if plain.HasGit() {
		t.Error("HasGit() = true for a non-git target")
	}
}

func TestInitialSync(t *testing.T) {
	m, origin, revs := newFixture(t)
	// A second git target whose repository does not exist.
	extra := []config.TargetConfig{
		{Name: "bad", Path: filepath.Join(t.TempDir(), "checkout"), Git: config.GitConfig{URL: filepath.Join(t.TempDir(), "missing.git"), Branch: "main"}, WorkingDir: t.TempDir()},
	}
	m = New(append(m.Targets(), extra...), zap.NewNop())

	err := m.InitialSync(context.Background())
	if err == nil {
		t.Fatal("InitialSync() error = nil, want the bad target's failure")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("InitialSync() error = %v, want it to name the failing target", err)
	}

	// The good target was loaded despite the other failing.
	tgt, ok := m.Target("one")
	if !ok {
		t.Fatal("Target(one) = not found")
	}
	if len(tgt.Playbooks) != 1 || tgt.Playbooks[0].Name != "site" {
		t.Errorf("good target playbooks = %+v, want [site]", tgt.Playbooks)
	}
	if len(tgt.Notifications) != 1 || tgt.Notifications[0].URL != "https://example.com/hook" {
		t.Errorf("good target notifications were lost on sync: %+v", tgt.Notifications)
	}
	if tgt.Org != "lab" {
		t.Errorf("Org = %q, want lab (preserved across sync)", tgt.Org)
	}

	st, ok := m.GitStatus("one")
	if !ok || st.Rev != revs["main"] || st.Ref != "main" || !st.Cloned || st.URL != origin {
		t.Errorf("GitStatus(one) = %+v, want cloned main at %s", st, revs["main"])
	}
}

func TestSyncUnknownAndNotGit(t *testing.T) {
	m, _, _ := newFixture(t)
	plain := New(append(m.Targets(), config.TargetConfig{Name: "local", Path: t.TempDir()}), zap.NewNop())

	_, _, err := plain.Sync(context.Background(), "ghost", "")
	if !errors.Is(err, ErrUnknownTarget) {
		t.Errorf("Sync(ghost) error = %v, want ErrUnknownTarget", err)
	}
	_, _, err = plain.Sync(context.Background(), "local", "")
	if !errors.Is(err, ErrNotGit) {
		t.Errorf("Sync(local) error = %v, want ErrNotGit", err)
	}
}

func TestSyncBranchSwitch(t *testing.T) {
	m, _, revs := newFixture(t)
	if err := m.InitialSync(context.Background()); err != nil {
		t.Fatalf("InitialSync() error: %v", err)
	}

	ctx := context.Background()
	rev, changed, err := m.Sync(ctx, "one", "feature")
	if err != nil {
		t.Fatalf("Sync(feature) error: %v", err)
	}
	if rev != revs["feature"] || !changed {
		t.Errorf("Sync(feature) = %q %v, want %q true", rev, changed, revs["feature"])
	}

	tgt, _ := m.Target("one")
	if len(tgt.Playbooks) != 1 || tgt.Playbooks[0].Name != "feature" {
		t.Fatalf("playbooks after switch = %+v, want [feature]", tgt.Playbooks)
	}
	// The branch ref is substituted into the templated inventory.
	if got, want := tgt.Playbooks[0].Inventory, "inventories/feature.ini"; got != want {
		t.Errorf("templated inventory = %q, want %q", got, want)
	}

	// A second sync without new commits reports no change, but the ref is
	// kept.
	rev, changed, err = m.Sync(ctx, "one", "")
	if err != nil {
		t.Fatalf("Sync(no branch) error: %v", err)
	}
	if changed || rev != revs["feature"] {
		t.Errorf("Sync(no branch) = %q %v, want %q false", rev, changed, revs["feature"])
	}

	st, _ := m.GitStatus("one")
	if st.Ref != "feature" || st.Rev != revs["feature"] {
		t.Errorf("GitStatus(one) = %+v, want ref feature at %s", st, revs["feature"])
	}
}

func TestSyncReloadFailureKeepsOldPlaybooks(t *testing.T) {
	m, origin, _ := newFixture(t)
	if err := m.InitialSync(context.Background()); err != nil {
		t.Fatalf("InitialSync() error: %v", err)
	}

	// Add a "broken" branch upstream: a strangle.yml without playbooks.
	orepo, err := gogit.PlainOpen(origin)
	if err != nil {
		t.Fatalf("open origin: %v", err)
	}
	mainRef, err := orepo.Reference(plumbing.ReferenceName("refs/heads/main"), true)
	if err != nil {
		t.Fatalf("resolve main: %v", err)
	}
	if err := orepo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("broken"), mainRef.Hash())); err != nil {
		t.Fatalf("create broken: %v", err)
	}
	if err := orepo.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("broken"))); err != nil {
		t.Fatalf("switch HEAD to broken: %v", err)
	}
	wt, err := orepo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	writeFixtureFile(t, origin, "strangle.yml", "working_dir: x\n")
	if _, err := wt.Add("strangle.yml"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := wt.Commit("broken", &gogit.CommitOptions{Author: testSig}); err != nil {
		t.Fatalf("commit broken: %v", err)
	}
	if err := orepo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("restore HEAD: %v", err)
	}
	orepo.Close()

	if _, _, err := m.Sync(context.Background(), "one", "broken"); err == nil {
		t.Fatal("Sync(broken) error = nil, want the strangle.yml load failure")
	}

	tgt, _ := m.Target("one")
	if len(tgt.Playbooks) != 1 || tgt.Playbooks[0].Name != "site" {
		t.Errorf("playbooks after failed reload = %+v, want the old [site]", tgt.Playbooks)
	}
}

func TestSyncGitFailure(t *testing.T) {
	m, _, _ := newFixture(t)
	if err := m.InitialSync(context.Background()); err != nil {
		t.Fatalf("InitialSync() error: %v", err)
	}

	// A ref that does not exist upstream fails the sync.
	_, _, err := m.Sync(context.Background(), "one", "no-such-branch")
	if err == nil {
		t.Fatal("Sync(no-such-branch) error = nil, want a fetch failure")
	}
}

func TestReload(t *testing.T) {
	m, _, _ := newFixture(t)
	if err := m.InitialSync(context.Background()); err != nil {
		t.Fatalf("InitialSync() error: %v", err)
	}

	// Simulate the master having updated the checkout: the worker re-reads
	// the strangle.yml without touching git.
	alt := `
playbooks:
  - name: site
    playbook: site.yml
    inventory: inventory.ini
  - name: extra
    playbook: extra.yml
    inventory: inventory.ini
`
	tgt, _ := m.Target("one")
	if err := os.WriteFile(filepath.Join(tgt.Path, "strangle.yml"), []byte(alt), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err != nil {
		t.Fatalf("Reload() error: %v", err)
	}
	tgt, _ = m.Target("one")
	if len(tgt.Playbooks) != 2 || tgt.Playbooks[1].Name != "extra" {
		t.Errorf("playbooks after reload = %+v, want [site extra]", tgt.Playbooks)
	}
}

func TestReloadFailureKeepsOldPlaybooks(t *testing.T) {
	m, _, _ := newFixture(t)
	if err := m.InitialSync(context.Background()); err != nil {
		t.Fatalf("InitialSync() error: %v", err)
	}

	tgt, _ := m.Target("one")
	if err := os.WriteFile(filepath.Join(tgt.Path, "strangle.yml"), []byte("working_dir: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(); err == nil {
		t.Fatal("Reload() error = nil, want the load failure")
	}
	tgt, _ = m.Target("one")
	if len(tgt.Playbooks) != 1 || tgt.Playbooks[0].Name != "site" {
		t.Errorf("playbooks after failed reload = %+v, want the old [site]", tgt.Playbooks)
	}
}
