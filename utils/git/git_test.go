package git

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
	"github.com/go-git/go-git/v6/plumbing/object"
	"go.uber.org/zap"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestGit(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Git Suite")
}

var testSig = &object.Signature{
	Name:  "strangle-test",
	Email: "strangle-test@example.com",
	When:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

func writeFixtureFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}

// newOrigin builds a local origin repository with go-git:
//
//	main:    c1 -> c3
//	feature: c1 -> c2
//	tag v1.0 on c1
//
// and returns the repository path and the revisions of the three commits.
// HEAD is left on main at c3.
func newOrigin() (string, map[string]string, error) {
	origin := filepath.Join(GinkgoT().TempDir(), "origin")
	repo, err := gogit.PlainInit(origin, false, gogit.WithDefaultBranch(plumbing.ReferenceName("refs/heads/main")))
	if err != nil {
		return "", nil, fmt.Errorf("init origin: %w", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", nil, err
	}
	commit := func(content, msg string) (string, error) {
		if err := writeFixtureFile(origin, "file.txt", content); err != nil {
			return "", fmt.Errorf("write %s: %w", "file.txt", err)
		}
		if _, err := wt.Add("file.txt"); err != nil {
			return "", fmt.Errorf("add: %w", err)
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: testSig})
		if err != nil {
			return "", fmt.Errorf("commit %s: %w", msg, err)
		}
		return h.String(), nil
	}
	setBranch := func(name string) error {
		if err := repo.Storer.SetReference(
			plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(name))); err != nil {
			return fmt.Errorf("set HEAD to %s: %w", name, err)
		}
		return nil
	}

	c1, err := commit("a\n", "c1")
	if err != nil {
		return "", nil, err
	}
	if err := repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), plumbing.NewHash(c1))); err != nil {
		return "", nil, fmt.Errorf("create feature: %w", err)
	}
	if err := setBranch("feature"); err != nil {
		return "", nil, err
	}
	c2, err := commit("b\n", "c2")
	if err != nil {
		return "", nil, err
	}
	if err := setBranch("main"); err != nil {
		return "", nil, err
	}
	c3, err := commit("c\n", "c3")
	if err != nil {
		return "", nil, err
	}
	if _, err := repo.CreateTag("v1.0", plumbing.NewHash(c1), nil); err != nil {
		return "", nil, fmt.Errorf("create tag: %w", err)
	}
	if err := repo.Close(); err != nil {
		return "", nil, fmt.Errorf("close origin: %w", err)
	}
	return origin, map[string]string{"c1": c1, "c2": c2, "c3": c3}, nil
}

// commitToOrigin appends a commit with the given file content to the current
// branch of an existing origin repository and returns its revision.
func commitToOrigin(origin, content, msg string) (string, error) {
	repo, err := gogit.PlainOpen(origin)
	if err != nil {
		return "", fmt.Errorf("open origin: %w", err)
	}
	defer repo.Close()
	wt, err := repo.Worktree()
	if err != nil {
		return "", err
	}
	if err := writeFixtureFile(origin, "file.txt", content); err != nil {
		return "", fmt.Errorf("write %s: %w", "file.txt", err)
	}
	if _, err := wt.Add("file.txt"); err != nil {
		return "", fmt.Errorf("add: %w", err)
	}
	h, err := wt.Commit(msg, &gogit.CommitOptions{Author: testSig})
	if err != nil {
		return "", fmt.Errorf("commit %s: %w", msg, err)
	}
	return h.String(), nil
}

var _ = Describe("SyncCloneAndUpdate", func() {
	It("clones on the first sync, follows new upstream commits and resets local drift", func() {
		origin, revs, err := newOrigin()
		Expect(err).NotTo(HaveOccurred())
		dir := filepath.Join(GinkgoT().TempDir(), "work")
		repo := New(origin, "main", dir, "", zap.NewNop())

		Expect(repo.IsCloned()).To(BeFalse(), "IsCloned() = true before any sync")

		ctx := context.Background()
		rev, changed, err := repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "first Sync() error")
		Expect(rev).To(Equal(revs["c3"]), "first Sync() = %q %v, want %q true", rev, changed, revs["c3"])
		Expect(changed).To(BeTrue(), "first Sync() = %q %v, want %q true", rev, changed, revs["c3"])
		Expect(repo.IsCloned()).To(BeTrue(), "IsCloned() = false after sync")
		branch, bRev, err := repo.Status(ctx)
		Expect(err).NotTo(HaveOccurred(), "Status() error")
		Expect(branch).To(Equal("main"), "Status() = %q %q, want main %q", branch, bRev, revs["c3"])
		Expect(bRev).To(Equal(revs["c3"]), "Status() = %q %q, want main %q", branch, bRev, revs["c3"])

		// No new commits: the revision is unchanged.
		rev, changed, err = repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "second Sync() error")
		Expect(changed).To(BeFalse(), "second Sync() = %q %v, want %q false", rev, changed, revs["c3"])
		Expect(rev).To(Equal(revs["c3"]), "second Sync() = %q %v, want %q false", rev, changed, revs["c3"])

		// A new upstream commit is picked up.
		c4, err := commitToOrigin(origin, "d\n", "c4")
		Expect(err).NotTo(HaveOccurred())
		rev, changed, err = repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "third Sync() error")
		Expect(rev).To(Equal(c4), "third Sync() = %q %v, want %q true", rev, changed, c4)
		Expect(changed).To(BeTrue(), "third Sync() = %q %v, want %q true", rev, changed, c4)

		// Local drift is reset away.
		Expect(writeFixtureFile(dir, "file.txt", "local drift\n")).NotTo(HaveOccurred())
		rev, changed, err = repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "fourth Sync() error")
		Expect(changed).To(BeFalse(), "fourth Sync() = %q %v, want %q false", rev, changed, c4)
		Expect(rev).To(Equal(c4), "fourth Sync() = %q %v, want %q false", rev, changed, c4)
		content, err := os.ReadFile(filepath.Join(dir, "file.txt"))
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(content))).To(Equal("d"), "working tree file = %q, want the upstream content", string(content))
	})
})

var _ = Describe("SyncBranchSwitch", func() {
	It("switches to another branch on demand", func() {
		origin, revs, err := newOrigin()
		Expect(err).NotTo(HaveOccurred())
		dir := filepath.Join(GinkgoT().TempDir(), "work")
		repo := New(origin, "main", dir, "", zap.NewNop())

		ctx := context.Background()
		_, _, err = repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "Sync() error")

		repo.SetRef("feature")
		rev, changed, err := repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "Sync(feature) error")
		Expect(rev).To(Equal(revs["c2"]), "Sync(feature) = %q %v, want %q true", rev, changed, revs["c2"])
		Expect(changed).To(BeTrue(), "Sync(feature) = %q %v, want %q true", rev, changed, revs["c2"])
		Expect(repo.Ref()).To(Equal("feature"), "Ref() = %q, want feature", repo.Ref())
		branch, _, err := repo.Status(ctx)
		Expect(err).NotTo(HaveOccurred(), "Status() error")
		Expect(branch).To(Equal("feature"), "Status() branch = %q, want feature", branch)
	})
})

var _ = Describe("SyncTagCheckout", func() {
	It("checks out a tag in detached state", func() {
		origin, revs, err := newOrigin()
		Expect(err).NotTo(HaveOccurred())
		dir := filepath.Join(GinkgoT().TempDir(), "work")
		repo := New(origin, "main", dir, "", zap.NewNop())

		ctx := context.Background()
		_, _, err = repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "Sync() error")

		repo.SetRef("v1.0")
		rev, changed, err := repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "Sync(v1.0) error")
		Expect(rev).To(Equal(revs["c1"]), "Sync(v1.0) = %q %v, want %q true", rev, changed, revs["c1"])
		Expect(changed).To(BeTrue(), "Sync(v1.0) = %q %v, want %q true", rev, changed, revs["c1"])
		branch, _, err := repo.Status(ctx)
		Expect(err).NotTo(HaveOccurred(), "Status() error")
		Expect(branch).To(Equal("HEAD"), "Status() branch = %q, want HEAD (detached)", branch)
	})
})

var _ = Describe("SyncCommitSHA", func() {
	It("tracks the default branch for an empty ref and pins to a commit", func() {
		origin, revs, err := newOrigin()
		Expect(err).NotTo(HaveOccurred())
		dir := filepath.Join(GinkgoT().TempDir(), "work")
		repo := New(origin, "", dir, "", zap.NewNop())

		ctx := context.Background()
		rev, changed, err := repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "Sync() error")
		// An empty ref tracks the default branch.
		Expect(rev).To(Equal(revs["c3"]), "Sync() = %q %v, want %q true", rev, changed, revs["c3"])
		Expect(changed).To(BeTrue(), "Sync() = %q %v, want %q true", rev, changed, revs["c3"])
		branch, _, err := repo.Status(ctx)
		Expect(err).NotTo(HaveOccurred(), "Status() error")
		Expect(branch).To(Equal("main"), "Status() branch = %q, want main", branch)

		// Pin to a specific commit.
		repo.SetRef(revs["c2"])
		rev, changed, err = repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "Sync(sha) error")
		Expect(rev).To(Equal(revs["c2"]), "Sync(sha) = %q %v, want %q true", rev, changed, revs["c2"])
		Expect(changed).To(BeTrue(), "Sync(sha) = %q %v, want %q true", rev, changed, revs["c2"])
		branch, _, err = repo.Status(ctx)
		Expect(err).NotTo(HaveOccurred(), "Status() error")
		Expect(branch).To(Equal("HEAD"), "Status() branch = %q, want HEAD (detached)", branch)
	})
})

var _ = Describe("SyncDefaultBranchTracking", func() {
	It("follows new commits on the remote default branch", func() {
		origin, _, err := newOrigin()
		Expect(err).NotTo(HaveOccurred())
		dir := filepath.Join(GinkgoT().TempDir(), "work")
		repo := New(origin, "", dir, "", zap.NewNop())

		ctx := context.Background()
		_, _, err = repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "Sync() error")

		// A new commit on the remote default branch is followed.
		c4, err := commitToOrigin(origin, "d\n", "c4")
		Expect(err).NotTo(HaveOccurred())
		rev, changed, err := repo.Sync(ctx)
		Expect(err).NotTo(HaveOccurred(), "Sync() error")
		Expect(rev).To(Equal(c4), "Sync() = %q %v, want %q true", rev, changed, c4)
		Expect(changed).To(BeTrue(), "Sync() = %q %v, want %q true", rev, changed, c4)
	})
})

var _ = Describe("SyncCloneFailure", func() {
	It("reports a clone failure without a checkout", func() {
		dir := filepath.Join(GinkgoT().TempDir(), "work")
		repo := New(filepath.Join(GinkgoT().TempDir(), "missing.git"), "main", dir, "", zap.NewNop())

		_, _, err := repo.Sync(context.Background())
		Expect(err).To(HaveOccurred(), "Sync() error = nil, want a clone failure")
		Expect(repo.IsCloned()).To(BeFalse(), "IsCloned() = true after a failed clone")
	})
})

var _ = Describe("SyncUnknownRef", func() {
	It("fails to sync an unknown ref", func() {
		origin, _, err := newOrigin()
		Expect(err).NotTo(HaveOccurred())
		dir := filepath.Join(GinkgoT().TempDir(), "work")
		repo := New(origin, "no-such-branch", dir, "", zap.NewNop())

		_, _, err = repo.Sync(context.Background())
		Expect(err).To(HaveOccurred(), "Sync() error = nil, want an unknown-ref failure")
		Expect(err.Error()).To(ContainSubstring("is not a branch, tag or commit"), "Sync() error = %v, want an unknown-ref failure", err)
	})
})

var _ = Describe("StatusNotCloned", func() {
	It("errors for a missing checkout", func() {
		repo := New("unused", "main", GinkgoT().TempDir(), "", zap.NewNop())
		_, _, err := repo.Status(context.Background())
		Expect(err).To(HaveOccurred(), "Status() error = nil, want an error for a missing checkout")
	})
})

var _ = Describe("Accessors", func() {
	It("exposes the URL, dir and ref", func() {
		repo := New("https://example.com/o/r.git", "main", "/tmp/work", "tok", zap.NewNop())
		Expect(repo.URL()).To(Equal("https://example.com/o/r.git"), "URL() = %q", repo.URL())
		Expect(repo.Dir()).To(Equal("/tmp/work"), "Dir() = %q", repo.Dir())
		Expect(repo.Ref()).To(Equal("main"), "Ref() = %q", repo.Ref())
		repo.SetRef("develop")
		Expect(repo.Ref()).To(Equal("develop"), "Ref() after SetRef = %q, want develop", repo.Ref())
	})
})

var _ = Describe("ClientOptions", func() {
	cases := []struct {
		name  string
		url   string
		token string
		want  int
	}{
		{"https with token", "https://github.com/o/r.git", "sekrit", 1},
		{"https without token", "https://github.com/o/r.git", "", 0},
		{"plain http with token", "http://github.com/o/r.git", "sekrit", 0},
		{"local path with token", "/tmp/origin", "sekrit", 0},
	}
	for _, tc := range cases {
		It(tc.name, func() {
			r := New(tc.url, "", GinkgoT().TempDir(), tc.token, zap.NewNop())
			Expect(len(r.clientOptions())).To(Equal(tc.want), "len(clientOptions()) = %d, want %d", len(r.clientOptions()), tc.want)
		})
	}
})

var _ = Describe("RedactError", func() {
	It("replaces the token in errors and leaves plain errors alone", func() {
		r := New("u", "", "", "sekrit-token", zap.NewNop())
		err := r.redactError(errors.New("fatal: using sekrit-token"))
		Expect(err.Error()).To(ContainSubstring("[REDACTED]"), "redactError() = %v, want the token replaced", err)
		Expect(err.Error()).ToNot(ContainSubstring("sekrit-token"), "redactError() = %v, want the token replaced", err)

		plain := New("u", "", "", "", zap.NewNop())
		err = plain.redactError(errors.New("fatal: plain"))
		Expect(err.Error()).To(ContainSubstring("plain"), "redactError() = %v, want unchanged without a token", err)
	})
})

var _ = Describe("SyncTokenNotLeaked", func() {
	It("never leaks the token in sync errors", func() {
		const token = "super-secret-token-xyz"
		repo := New("https://127.0.0.1:9/x/y.git", "main", filepath.Join(GinkgoT().TempDir(), "work"), token, zap.NewNop())

		_, _, err := repo.Sync(context.Background())
		Expect(err).To(HaveOccurred(), "Sync() error = nil, want a clone failure")
		Expect(err.Error()).ToNot(ContainSubstring(token), "Sync() error leaks the token: %v", err)
	})
})
