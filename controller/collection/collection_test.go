package collection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"go.uber.org/zap"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/kakwa/strangle/utils/cfg"
)

var testSig = &plumbing.Signature{
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
func writeFixtureFile(dir, name, content string) error {
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// newOrigin builds a local origin repository with go-git:
//
//	main:    strangle.yml (playbook "site")
//	feature: strangle.yml (playbook "feature", branch-templated inventory)
//	broken:  a strangle.yml without playbooks
//
// and returns the repository path and the branch heads. HEAD is left on
// main.
func newOrigin() (string, map[string]string) {
	origin := filepath.Join(GinkgoT().TempDir(), "origin")
	repo, err := gogit.PlainInit(origin, false, gogit.WithDefaultBranch(plumbing.ReferenceName("refs/heads/main")))
	if err != nil {
		Failf("init origin: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		Failf("worktree: %v", err)
	}
	commit := func(msg string, files map[string]string) string {
		for name, content := range files {
			Expect(writeFixtureFile(origin, name, content)).To(Succeed(), name)
		}
		for name := range files {
			if _, err := wt.Add(name); err != nil {
				Failf("add %s: %v", name, err)
			}
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: testSig})
		if err != nil {
			Failf("commit %s: %v", msg, err)
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
		Failf("create feature: %v", err)
	}
	if err := repo.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("feature"))); err != nil {
		Failf("switch HEAD to feature: %v", err)
	}
	featureHead := commit("feature", map[string]string{"strangle.yml": featureStrangle})
	if err := repo.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		Failf("switch HEAD back to main: %v", err)
	}
	if err := repo.Close(); err != nil {
		Failf("close origin: %v", err)
	}
	return origin, map[string]string{"main": mainHead, "feature": featureHead}
}

// newFixture builds the origin plus a Manager for a git-backed target "one"
// checked out next to the origin. It returns the manager, the origin path
// and the branch heads.
func newFixture() (*Manager, string, map[string]string) {
	origin, revs := newOrigin()
	checkoutDir := filepath.Join(GinkgoT().TempDir(), "checkout")
	targets := []cfg.TargetConfig{{
		Name:       "one",
		Org:        "lab",
		Path:       checkoutDir,
		Git:        cfg.GitConfig{URL: origin, Branch: "main"},
		WorkingDir: checkoutDir,
	}}
	m := New(targets, zap.NewNop())
	return m, origin, revs
}

func TestCollection(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Collection Suite")
}

var _ = Describe("NewAndTargets", func() {
	It("exposes git support and sorted targets", func() {
		m := New([]cfg.TargetConfig{
			{Name: "b", Path: "/b"},
			{Name: "a", Path: "/a", Git: cfg.GitConfig{URL: "https://example.com/a.git"}},
		}, zap.NewNop())

		Expect(m.HasGit()).To(BeTrue())
		got := m.Targets()
		Expect(got).To(HaveLen(2))
		Expect(got[0].Name).To(Equal("a"))
		Expect(got[1].Name).To(Equal("b"))
		_, ok := m.Target("b")
		Expect(ok).To(BeTrue(), "Target(b) = not found")
		_, ok = m.Target("nope")
		Expect(ok).To(BeFalse(), "Target(nope) = found, want not found")

		plain := New([]cfg.TargetConfig{{Name: "a", Path: "/a"}}, zap.NewNop())
		Expect(plain.HasGit()).To(BeFalse(), "HasGit() = true for a non-git target")
	})
})

var _ = Describe("InitialSync", func() {
	It("loads the good targets despite a failing one", func() {
		m, origin, revs := newFixture()
		// A second git target whose repository does not exist.
		extra := []cfg.TargetConfig{
			{Name: "bad", Path: filepath.Join(GinkgoT().TempDir(), "checkout"), Git: cfg.GitConfig{URL: filepath.Join(GinkgoT().TempDir(), "missing.git"), Branch: "main"}, WorkingDir: GinkgoT().TempDir()},
		}
		m = New(append(m.Targets(), extra...), zap.NewNop())

		err := m.InitialSync(context.Background())
		Expect(err).To(HaveOccurred(), "InitialSync() error = nil, want the bad target's failure")
		Expect(err.Error()).To(ContainSubstring("bad"), "InitialSync() error = %v, want it to name the failing target", err)

		// The good target was loaded despite the other failing.
		tgt, ok := m.Target("one")
		Expect(ok).To(BeTrue(), "Target(one) = not found")
		Expect(tgt.Playbooks).To(HaveLen(1), "good target playbooks = %+v, want [site]", tgt.Playbooks)
		Expect(tgt.Playbooks[0].Name).To(Equal("site"))
		Expect(tgt.Org).To(Equal("lab"), "Org = %q, want lab (preserved across sync)", tgt.Org)

		st, ok := m.GitStatus("one")
		Expect(ok).To(BeTrue(), "GitStatus(one) = %+v, want cloned main at %s", st, revs["main"])
		Expect(st.Rev).To(Equal(revs["main"]), "GitStatus(one) = %+v, want cloned main at %s", st, revs["main"])
		Expect(st.Ref).To(Equal("main"), "GitStatus(one) = %+v, want cloned main at %s", st, revs["main"])
		Expect(st.Cloned).To(BeTrue(), "GitStatus(one) = %+v, want cloned main at %s", st, revs["main"])
		Expect(st.URL).To(Equal(origin), "GitStatus(one) = %+v, want cloned main at %s", st, revs["main"])
	})
})

var _ = Describe("SyncUnknownAndNotGit", func() {
	It("rejects unknown and non-git targets", func() {
		m, _, _ := newFixture()
		plain := New(append(m.Targets(), cfg.TargetConfig{Name: "local", Path: GinkgoT().TempDir()}), zap.NewNop())

		_, _, err := plain.Sync(context.Background(), "ghost", "")
		Expect(errors.Is(err, ErrUnknownTarget)).To(BeTrue(), "Sync(ghost) error = %v, want ErrUnknownTarget", err)
		_, _, err = plain.Sync(context.Background(), "local", "")
		Expect(errors.Is(err, ErrNotGit)).To(BeTrue(), "Sync(local) error = %v, want ErrNotGit", err)
	})
})

var _ = Describe("SyncBranchSwitch", func() {
	It("switches to a feature branch and keeps the ref", func() {
		m, _, revs := newFixture()
		Expect(m.InitialSync(context.Background())).To(Succeed(), "InitialSync() error")

		ctx := context.Background()
		rev, changed, err := m.Sync(ctx, "one", "feature")
		Expect(err).NotTo(HaveOccurred(), "Sync(feature) error")
		Expect(rev).To(Equal(revs["feature"]), "Sync(feature) = %q %v, want %q true", rev, changed, revs["feature"])
		Expect(changed).To(BeTrue(), "Sync(feature) = %q %v, want %q true", rev, changed, revs["feature"])

		tgt, _ := m.Target("one")
		Expect(tgt.Playbooks).To(HaveLen(1), "playbooks after switch = %+v, want [feature]", tgt.Playbooks)
		Expect(tgt.Playbooks[0].Name).To(Equal("feature"))
		// The branch ref is substituted into the templated inventory.
		Expect(tgt.Playbooks[0].Inventory).To(Equal("inventories/feature.ini"))

		// A second sync without new commits reports no change, but the ref is
		// kept.
		rev, changed, err = m.Sync(ctx, "one", "")
		Expect(err).NotTo(HaveOccurred(), "Sync(no branch) error")
		Expect(changed).To(BeFalse(), "Sync(no branch) = %q %v, want %q false", rev, changed, revs["feature"])
		Expect(rev).To(Equal(revs["feature"]), "Sync(no branch) = %q %v, want %q false", rev, changed, revs["feature"])

		st, _ := m.GitStatus("one")
		Expect(st.Ref).To(Equal("feature"), "GitStatus(one) = %+v, want ref feature at %s", st, revs["feature"])
		Expect(st.Rev).To(Equal(revs["feature"]), "GitStatus(one) = %+v, want ref feature at %s", st, revs["feature"])
	})
})

var _ = Describe("SyncReloadFailureKeepsOldPlaybooks", func() {
	It("keeps the old playbooks when the new branch fails to load", func() {
		m, origin, _ := newFixture()
		Expect(m.InitialSync(context.Background())).To(Succeed(), "InitialSync() error")

		// Add a "broken" branch upstream: a strangle.yml without playbooks.
		orepo, err := gogit.PlainOpen(origin)
		Expect(err).NotTo(HaveOccurred(), "open origin")
		mainRef, err := orepo.Reference(plumbing.ReferenceName("refs/heads/main"), true)
		Expect(err).NotTo(HaveOccurred(), "resolve main")
		Expect(orepo.Storer.SetReference(
			plumbing.NewHashReference(plumbing.NewBranchReferenceName("broken"), mainRef.Hash()))).To(Succeed(), "create broken")
		Expect(orepo.Storer.SetReference(
			plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("broken")))).To(Succeed(), "switch HEAD to broken")
		wt, err := orepo.Worktree()
		Expect(err).NotTo(HaveOccurred(), "worktree")
		Expect(writeFixtureFile(origin, "strangle.yml", "working_dir: x\n")).To(Succeed())
		_, err = wt.Add("strangle.yml")
		Expect(err).NotTo(HaveOccurred(), "add")
		_, err = wt.Commit("broken", &gogit.CommitOptions{Author: testSig})
		Expect(err).NotTo(HaveOccurred(), "commit broken")
		Expect(orepo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main")))).To(Succeed(), "restore HEAD")
		orepo.Close()

		_, _, err = m.Sync(context.Background(), "one", "broken")
		Expect(err).To(HaveOccurred(), "Sync(broken) error = nil, want the strangle.yml load failure")

		tgt, _ := m.Target("one")
		Expect(tgt.Playbooks).To(HaveLen(1), "playbooks after failed reload = %+v, want the old [site]", tgt.Playbooks)
		Expect(tgt.Playbooks[0].Name).To(Equal("site"))
	})
})

var _ = Describe("SyncGitFailure", func() {
	It("fails the sync when the branch does not exist upstream", func() {
		m, _, _ := newFixture()
		Expect(m.InitialSync(context.Background())).To(Succeed(), "InitialSync() error")

		// A ref that does not exist upstream fails the sync.
		_, _, err := m.Sync(context.Background(), "one", "no-such-branch")
		Expect(err).To(HaveOccurred(), "Sync(no-such-branch) error = nil, want a fetch failure")
	})
})

var _ = Describe("Reload", func() {
	It("re-reads the strangle.yml without touching git", func() {
		m, _, _ := newFixture()
		Expect(m.InitialSync(context.Background())).To(Succeed(), "InitialSync() error")

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
		Expect(os.WriteFile(filepath.Join(tgt.Path, "strangle.yml"), []byte(alt), 0o644)).To(Succeed())
		Expect(m.Reload()).To(Succeed(), "Reload() error")
		tgt, _ = m.Target("one")
		Expect(tgt.Playbooks).To(HaveLen(2), "playbooks after reload = %+v, want [site extra]", tgt.Playbooks)
		Expect(tgt.Playbooks[1].Name).To(Equal("extra"))
	})
})

var _ = Describe("ReloadFailureKeepsOldPlaybooks", func() {
	It("keeps the old playbooks when the reload fails", func() {
		m, _, _ := newFixture()
		Expect(m.InitialSync(context.Background())).To(Succeed(), "InitialSync() error")

		tgt, _ := m.Target("one")
		Expect(os.WriteFile(filepath.Join(tgt.Path, "strangle.yml"), []byte("working_dir: x\n"), 0o644)).To(Succeed())
		Expect(m.Reload()).To(HaveOccurred(), "Reload() error = nil, want the load failure")
		tgt, _ = m.Target("one")
		Expect(tgt.Playbooks).To(HaveLen(1), "playbooks after failed reload = %+v, want the old [site]", tgt.Playbooks)
		Expect(tgt.Playbooks[0].Name).To(Equal("site"))
	})
})
