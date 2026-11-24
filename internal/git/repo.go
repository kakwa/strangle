// Package git manages a local checkout of a remote repository: it clones the
// repository on first use and keeps the checkout pinned to a tracked ref
// (branch, tag or commit) on every sync.
//
// The package is built on go-git and does not require a system git binary.
package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"go.uber.org/zap"
)

// remoteName is the name the remote is registered as inside the checkout.
const remoteName = "origin"

// Repo is a local checkout of a remote repository, pinned to a tracked ref.
type Repo struct {
	url    string
	ref    string
	dir    string
	token  string
	logger *zap.Logger
}

// New returns a Repo for the given remote. dir is the directory the checkout
// is kept in; ref is the tracked branch, tag or commit. token is an optional
// credential for https remotes.
func New(u, ref, dir, token string, logger *zap.Logger) *Repo {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Repo{url: u, ref: ref, dir: dir, token: token, logger: logger}
}

// URL is the remote repository URL, without credentials.
func (r *Repo) URL() string { return r.url }

// Dir is the directory holding the checkout.
func (r *Repo) Dir() string { return r.dir }

// Ref is the tracked ref.
func (r *Repo) Ref() string { return r.ref }

// SetRef changes the tracked ref, e.g. when a sync request switches the
// target to another branch.
func (r *Repo) SetRef(ref string) { r.ref = ref }

// IsCloned reports whether dir holds a git checkout.
func (r *Repo) IsCloned() bool {
	_, err := os.Stat(filepath.Join(r.dir, ".git"))
	return err == nil
}

// Sync clones the repository (first sync) or updates the checkout to the
// tracked ref. An empty ref tracks the remote's default branch. It returns
// the new HEAD revision and whether the revision changed since the previous
// sync.
func (r *Repo) Sync(ctx context.Context) (string, bool, error) {
	repo, oldRev, hadOld, err := r.openOrClone(ctx)
	if err != nil {
		return "", false, err
	}
	defer repo.Close()

	if r.ref != "" {
		err = r.checkoutRef(ctx, repo)
	} else {
		// No tracked ref: follow the remote's default branch.
		err = r.trackDefaultBranch(ctx, repo)
	}
	if err != nil {
		return "", false, err
	}

	rev, err := headRev(repo)
	if err != nil {
		return "", false, err
	}
	changed := !hadOld || rev != oldRev
	r.logger.Info("git synced",
		zap.String("url", r.url),
		zap.String("ref", r.ref),
		zap.String("rev", rev),
		zap.Bool("changed", changed))
	return rev, changed, nil
}

// Status reports the checkout's current branch (or "HEAD" when detached)
// and its HEAD revision.
func (r *Repo) Status(_ context.Context) (branch, rev string, err error) {
	repo, err := r.open()
	if err != nil {
		return "", "", err
	}
	defer repo.Close()

	rev, err = headRev(repo)
	if err != nil {
		return "", "", err
	}
	branch = "HEAD"
	if head, herr := repo.Storer.Reference(plumbing.HEAD); herr == nil && head.Type() == plumbing.SymbolicReference && head.Target().IsBranch() {
		branch = head.Target().Short()
	}
	return branch, rev, nil
}

// openOrClone opens the checkout if it exists, cloning it otherwise. For an
// existing checkout it also returns the current HEAD revision, if any.
func (r *Repo) openOrClone(ctx context.Context) (*git.Repository, string, bool, error) {
	if !r.IsCloned() {
		repo, err := r.clone(ctx)
		if err != nil {
			return nil, "", false, err
		}
		return repo, "", false, nil
	}
	repo, err := r.open()
	if err != nil {
		return nil, "", false, err
	}
	oldRev, hadOld := "", false
	if rev, err := headRev(repo); err == nil {
		oldRev, hadOld = rev, true
	}
	return repo, oldRev, hadOld, nil
}

// clone clones the remote into r.dir. The directory must not exist or be
// empty. All branches and tags are fetched so that any tracked ref can be
// resolved on later syncs.
func (r *Repo) clone(ctx context.Context) (*git.Repository, error) {
	repo, err := git.PlainCloneContext(ctx, r.dir, &git.CloneOptions{
		URL:           r.url,
		RemoteName:    remoteName,
		Tags:          plumbing.AllTags,
		ClientOptions: r.clientOptions(),
	})
	if err != nil {
		return nil, r.redactError(fmt.Errorf("git clone: %w", err))
	}
	return repo, nil
}

// open opens the checkout in r.dir.
func (r *Repo) open() (*git.Repository, error) {
	repo, err := git.PlainOpen(r.dir)
	if err != nil {
		return nil, r.redactError(fmt.Errorf("open repository: %w", err))
	}
	return repo, nil
}

// checkoutRef moves the checkout onto the tracked ref: a remote branch is
// pinned to a local branch of the same name, a tag or commit is checked out
// in detached state.
func (r *Repo) checkoutRef(ctx context.Context, repo *git.Repository) error {
	remote, err := repo.Remote(remoteName)
	if err != nil {
		return err
	}
	refs, err := remote.ListContext(ctx, &git.ListOptions{ClientOptions: r.clientOptions()})
	if err != nil {
		return r.redactError(fmt.Errorf("git ls-remote: %w", err))
	}

	var hash plumbing.Hash
	var branch string
	switch {
	case refIn(refs, "refs/heads/"+r.ref):
		if err := r.fetch(ctx, repo, "+refs/heads/"+r.ref+":refs/remotes/origin/"+r.ref); err != nil {
			return err
		}
		ref, err := repo.Reference(plumbing.ReferenceName("refs/remotes/origin/"+r.ref), true)
		if err != nil {
			return fmt.Errorf("resolve origin/%s: %w", r.ref, err)
		}
		hash, branch = ref.Hash(), r.ref
	case refIn(refs, "refs/tags/"+r.ref):
		if err := r.fetch(ctx, repo, "+refs/tags/"+r.ref+":refs/tags/"+r.ref); err != nil {
			return err
		}
		ref, err := repo.Reference(plumbing.ReferenceName("refs/tags/"+r.ref), true)
		if err != nil {
			return fmt.Errorf("resolve tag %s: %w", r.ref, err)
		}
		hash = ref.Hash()
	default:
		// A commit: resolve it against the local object store, fetching all
		// remote refs first if the commit is not there yet.
		h, err := repo.ResolveRevision(plumbing.Revision(r.ref))
		if err != nil {
			if ferr := r.fetchAll(ctx, repo); ferr != nil {
				return ferr
			}
			if h, err = repo.ResolveRevision(plumbing.Revision(r.ref)); err != nil {
				return fmt.Errorf("ref %q is not a branch, tag or commit", r.ref)
			}
		}
		hash = *h
	}

	if branch != "" {
		return r.checkoutBranch(repo, branch, hash)
	}
	return r.checkoutDetached(repo, hash)
}

// trackDefaultBranch follows the remote's default branch (read from the
// remote's HEAD advertisement) and keeps a local branch of the same name
// pinned to it.
func (r *Repo) trackDefaultBranch(ctx context.Context, repo *git.Repository) error {
	remote, err := repo.Remote(remoteName)
	if err != nil {
		return err
	}
	refs, err := remote.ListContext(ctx, &git.ListOptions{ClientOptions: r.clientOptions()})
	if err != nil {
		return r.redactError(fmt.Errorf("git ls-remote: %w", err))
	}
	var name string
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD {
			name = strings.TrimPrefix(ref.Target().String(), "refs/heads/")
			break
		}
	}
	if name == "" {
		return nil // no usable remote HEAD; leave the checkout where it is
	}
	if err := r.fetch(ctx, repo, "+refs/heads/"+name+":refs/remotes/origin/"+name); err != nil {
		return err
	}
	ref, err := repo.Reference(plumbing.ReferenceName("refs/remotes/origin/"+name), true)
	if err != nil {
		return fmt.Errorf("resolve origin/%s: %w", name, err)
	}
	return r.checkoutBranch(repo, name, ref.Hash())
}

// checkoutBranch pins the local branch name to the given commit and checks
// it out, the equivalent of `git checkout -B name origin/name`.
func (r *Repo) checkoutBranch(repo *git.Repository, name string, h plumbing.Hash) error {
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	local := plumbing.NewBranchReferenceName(name)
	if err := repo.Storer.SetReference(plumbing.NewHashReference(local, h)); err != nil {
		return fmt.Errorf("set branch ref: %w", err)
	}
	// A forced checkout discards any local drift so the working tree matches
	// the remote exactly.
	if err := wt.Checkout(&git.CheckoutOptions{Branch: local, Force: true}); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	return nil
}

// checkoutDetached checks out the given commit without a branch, the
// equivalent of `git checkout --detach <rev>`.
func (r *Repo) checkoutDetached(repo *git.Repository, h plumbing.Hash) error {
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Checkout(&git.CheckoutOptions{Hash: h, Force: true}); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}
	return nil
}

// fetch fetches one refspec from the remote. "Already up to date" is a
// successful outcome, not an error.
func (r *Repo) fetch(ctx context.Context, repo *git.Repository, refspec string) error {
	return r.fetchSpecs(ctx, repo, []config.RefSpec{config.RefSpec(refspec)})
}

// fetchAll refreshes all remote branches and tags.
func (r *Repo) fetchAll(ctx context.Context, repo *git.Repository) error {
	return r.fetchSpecs(ctx, repo, []config.RefSpec{
		config.RefSpec("+refs/heads/*:refs/remotes/origin/*"),
		config.RefSpec("+refs/tags/*:refs/tags/*"),
	})
}

func (r *Repo) fetchSpecs(ctx context.Context, repo *git.Repository, specs []config.RefSpec) error {
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName:    remoteName,
		RefSpecs:      specs,
		Force:         true,
		ClientOptions: r.clientOptions(),
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return r.redactError(fmt.Errorf("git fetch: %w", err))
	}
	return nil
}

func refIn(refs []*plumbing.Reference, name string) bool {
	for _, ref := range refs {
		if ref.Name().String() == name {
			return true
		}
	}
	return false
}

func headRev(repo *git.Repository) (string, error) {
	head, err := repo.Reference(plumbing.HEAD, true)
	if err != nil {
		return "", fmt.Errorf("resolve HEAD: %w", err)
	}
	return head.Hash().String(), nil
}

// clientOptions returns the go-git client options for the remote's transport:
// for https remotes the token is sent as HTTP basic-auth credentials.
func (r *Repo) clientOptions() []gitclient.Option {
	if r.token == "" || !strings.HasPrefix(r.url, "https://") {
		return nil
	}
	return []gitclient.Option{
		gitclient.WithHTTPAuth(&githttp.BasicAuth{Username: r.token, Password: ""}),
	}
}

// redactError removes the credential from an error message before it reaches
// logs or API responses.
func (r *Repo) redactError(err error) error {
	if r.token == "" {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), r.token, "[REDACTED]"))
}
