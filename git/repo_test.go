package git

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
)

var testSig = &object.Signature{
	Name:  "strangle-test",
	Email: "strangle-test@example.com",
	When:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
}

func writeFixtureFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// newOrigin builds a local origin repository with go-git:
//
//	main:    c1 -> c3
//	feature: c1 -> c2
//	tag v1.0 on c1
//
// and returns the repository path and the revisions of the three commits.
// HEAD is left on main at c3.
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
	commit := func(content, msg string) string {
		t.Helper()
		writeFixtureFile(t, origin, "file.txt", content)
		if _, err := wt.Add("file.txt"); err != nil {
			t.Fatalf("add: %v", err)
		}
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: testSig})
		if err != nil {
			t.Fatalf("commit %s: %v", msg, err)
		}
		return h.String()
	}
	setBranch := func(name string) {
		t.Helper()
		if err := repo.Storer.SetReference(
			plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(name))); err != nil {
			t.Fatalf("set HEAD to %s: %v", name, err)
		}
	}

	c1 := commit("a\n", "c1")
	if err := repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("feature"), plumbing.NewHash(c1))); err != nil {
		t.Fatalf("create feature: %v", err)
	}
	setBranch("feature")
	c2 := commit("b\n", "c2")
	setBranch("main")
	c3 := commit("c\n", "c3")
	if _, err := repo.CreateTag("v1.0", plumbing.NewHash(c1), nil); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if err := repo.Close(); err != nil {
		t.Fatalf("close origin: %v", err)
	}
	return origin, map[string]string{"c1": c1, "c2": c2, "c3": c3}
}

// commitToOrigin appends a commit with the given file content to the current
// branch of an existing origin repository and returns its revision.
func commitToOrigin(t *testing.T, origin, content, msg string) string {
	t.Helper()
	repo, err := gogit.PlainOpen(origin)
	if err != nil {
		t.Fatalf("open origin: %v", err)
	}
	defer repo.Close()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	writeFixtureFile(t, origin, "file.txt", content)
	if _, err := wt.Add("file.txt"); err != nil {
		t.Fatalf("add: %v", err)
	}
	h, err := wt.Commit(msg, &gogit.CommitOptions{Author: testSig})
	if err != nil {
		t.Fatalf("commit %s: %v", msg, err)
	}
	return h.String()
}

func TestSyncCloneAndUpdate(t *testing.T) {
	origin, revs := newOrigin(t)
	dir := filepath.Join(t.TempDir(), "work")
	repo := New(origin, "main", dir, "", zap.NewNop())

	if repo.IsCloned() {
		t.Fatal("IsCloned() = true before any sync")
	}

	ctx := context.Background()
	rev, changed, err := repo.Sync(ctx)
	if err != nil {
		t.Fatalf("first Sync() error: %v", err)
	}
	if rev != revs["c3"] || !changed {
		t.Errorf("first Sync() = %q %v, want %q true", rev, changed, revs["c3"])
	}
	if !repo.IsCloned() {
		t.Fatal("IsCloned() = false after sync")
	}
	branch, bRev, err := repo.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if branch != "main" || bRev != revs["c3"] {
		t.Errorf("Status() = %q %q, want main %q", branch, bRev, revs["c3"])
	}

	// No new commits: the revision is unchanged.
	rev, changed, err = repo.Sync(ctx)
	if err != nil {
		t.Fatalf("second Sync() error: %v", err)
	}
	if changed || rev != revs["c3"] {
		t.Errorf("second Sync() = %q %v, want %q false", rev, changed, revs["c3"])
	}

	// A new upstream commit is picked up.
	c4 := commitToOrigin(t, origin, "d\n", "c4")
	rev, changed, err = repo.Sync(ctx)
	if err != nil {
		t.Fatalf("third Sync() error: %v", err)
	}
	if rev != c4 || !changed {
		t.Errorf("third Sync() = %q %v, want %q true", rev, changed, c4)
	}

	// Local drift is reset away.
	writeFixtureFile(t, dir, "file.txt", "local drift\n")
	rev, changed, err = repo.Sync(ctx)
	if err != nil {
		t.Fatalf("fourth Sync() error: %v", err)
	}
	if changed || rev != c4 {
		t.Errorf("fourth Sync() = %q %v, want %q false", rev, changed, c4)
	}
	content, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(content)) != "d" {
		t.Errorf("working tree file = %q, want the upstream content", string(content))
	}
}

func TestSyncBranchSwitch(t *testing.T) {
	origin, revs := newOrigin(t)
	dir := filepath.Join(t.TempDir(), "work")
	repo := New(origin, "main", dir, "", zap.NewNop())

	ctx := context.Background()
	if _, _, err := repo.Sync(ctx); err != nil {
		t.Fatalf("Sync() error: %v", err)
	}

	repo.SetRef("feature")
	rev, changed, err := repo.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync(feature) error: %v", err)
	}
	if rev != revs["c2"] || !changed {
		t.Errorf("Sync(feature) = %q %v, want %q true", rev, changed, revs["c2"])
	}
	if repo.Ref() != "feature" {
		t.Errorf("Ref() = %q, want feature", repo.Ref())
	}
	branch, _, err := repo.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if branch != "feature" {
		t.Errorf("Status() branch = %q, want feature", branch)
	}
}

func TestSyncTagCheckout(t *testing.T) {
	origin, revs := newOrigin(t)
	dir := filepath.Join(t.TempDir(), "work")
	repo := New(origin, "main", dir, "", zap.NewNop())

	ctx := context.Background()
	if _, _, err := repo.Sync(ctx); err != nil {
		t.Fatalf("Sync() error: %v", err)
	}

	repo.SetRef("v1.0")
	rev, changed, err := repo.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync(v1.0) error: %v", err)
	}
	if rev != revs["c1"] || !changed {
		t.Errorf("Sync(v1.0) = %q %v, want %q true", rev, changed, revs["c1"])
	}
	branch, _, err := repo.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if branch != "HEAD" {
		t.Errorf("Status() branch = %q, want HEAD (detached)", branch)
	}
}

func TestSyncCommitSHA(t *testing.T) {
	origin, revs := newOrigin(t)
	dir := filepath.Join(t.TempDir(), "work")
	repo := New(origin, "", dir, "", zap.NewNop())

	ctx := context.Background()
	rev, changed, err := repo.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync() error: %v", err)
	}
	// An empty ref tracks the default branch.
	if rev != revs["c3"] || !changed {
		t.Errorf("Sync() = %q %v, want %q true", rev, changed, revs["c3"])
	}
	branch, _, err := repo.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if branch != "main" {
		t.Errorf("Status() branch = %q, want main", branch)
	}

	// Pin to a specific commit.
	repo.SetRef(revs["c2"])
	rev, changed, err = repo.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync(sha) error: %v", err)
	}
	if rev != revs["c2"] || !changed {
		t.Errorf("Sync(sha) = %q %v, want %q true", rev, changed, revs["c2"])
	}
	branch, _, err = repo.Status(ctx)
	if err != nil {
		t.Fatalf("Status() error: %v", err)
	}
	if branch != "HEAD" {
		t.Errorf("Status() branch = %q, want HEAD (detached)", branch)
	}
}

func TestSyncDefaultBranchTracking(t *testing.T) {
	origin, _ := newOrigin(t)
	dir := filepath.Join(t.TempDir(), "work")
	repo := New(origin, "", dir, "", zap.NewNop())

	ctx := context.Background()
	if _, _, err := repo.Sync(ctx); err != nil {
		t.Fatalf("Sync() error: %v", err)
	}

	// A new commit on the remote default branch is followed.
	c4 := commitToOrigin(t, origin, "d\n", "c4")
	rev, changed, err := repo.Sync(ctx)
	if err != nil {
		t.Fatalf("Sync() error: %v", err)
	}
	if rev != c4 || !changed {
		t.Errorf("Sync() = %q %v, want %q true", rev, changed, c4)
	}
}

func TestSyncCloneFailure(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	repo := New(filepath.Join(t.TempDir(), "missing.git"), "main", dir, "", zap.NewNop())

	_, _, err := repo.Sync(context.Background())
	if err == nil {
		t.Fatal("Sync() error = nil, want a clone failure")
	}
	if repo.IsCloned() {
		t.Error("IsCloned() = true after a failed clone")
	}
}

func TestSyncUnknownRef(t *testing.T) {
	origin, _ := newOrigin(t)
	dir := filepath.Join(t.TempDir(), "work")
	repo := New(origin, "no-such-branch", dir, "", zap.NewNop())

	_, _, err := repo.Sync(context.Background())
	if err == nil {
		t.Fatal("Sync() error = nil, want an unknown-ref failure")
	}
	if !strings.Contains(err.Error(), "is not a branch, tag or commit") {
		t.Errorf("Sync() error = %v, want an unknown-ref failure", err)
	}
}

func TestStatusNotCloned(t *testing.T) {
	repo := New("unused", "main", t.TempDir(), "", zap.NewNop())
	_, _, err := repo.Status(context.Background())
	if err == nil {
		t.Fatal("Status() error = nil, want an error for a missing checkout")
	}
}

func TestAccessors(t *testing.T) {
	repo := New("https://example.com/o/r.git", "main", "/tmp/work", "tok", zap.NewNop())
	if repo.URL() != "https://example.com/o/r.git" {
		t.Errorf("URL() = %q", repo.URL())
	}
	if repo.Dir() != "/tmp/work" {
		t.Errorf("Dir() = %q", repo.Dir())
	}
	if repo.Ref() != "main" {
		t.Errorf("Ref() = %q", repo.Ref())
	}
	repo.SetRef("develop")
	if repo.Ref() != "develop" {
		t.Errorf("Ref() after SetRef = %q, want develop", repo.Ref())
	}
}

func TestClientOptions(t *testing.T) {
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
		r := New(tc.url, "", t.TempDir(), tc.token, zap.NewNop())
		if got := len(r.clientOptions()); got != tc.want {
			t.Errorf("%s: len(clientOptions()) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRedactError(t *testing.T) {
	r := New("u", "", "", "sekrit-token", zap.NewNop())
	err := r.redactError(errors.New("fatal: using sekrit-token"))
	if !strings.Contains(err.Error(), "[REDACTED]") || strings.Contains(err.Error(), "sekrit-token") {
		t.Errorf("redactError() = %v, want the token replaced", err)
	}

	plain := New("u", "", "", "", zap.NewNop())
	err = plain.redactError(errors.New("fatal: plain"))
	if !strings.Contains(err.Error(), "plain") {
		t.Errorf("redactError() = %v, want unchanged without a token", err)
	}
}

func TestSyncTokenNotLeaked(t *testing.T) {
	const token = "super-secret-token-xyz"
	repo := New("https://127.0.0.1:9/x/y.git", "main", filepath.Join(t.TempDir(), "work"), token, zap.NewNop())

	_, _, err := repo.Sync(context.Background())
	if err == nil {
		t.Fatal("Sync() error = nil, want a clone failure")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("Sync() error leaks the token: %v", err)
	}
}
