// Package gitrepo_test holds gitrepo tests that need testutil. testutil imports
// gitrepo, so these cannot live in the gitrepo package itself.
package gitrepo_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
)

// newRepoWithCommit returns a repo directory holding one committed file.
func newRepoWithCommit(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "tracked.txt", "initial")
	testutil.GitAdd(t, dir, "tracked.txt")
	testutil.GitCommit(t, dir, "initial commit")

	return dir
}

func TestStatus_WithoutCacheSeesWorktreeChanges(t *testing.T) {
	t.Parallel()

	dir := newRepoWithCommit(t)
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath(%q) error = %v", dir, err)
	}
	defer repo.Close()

	ctx := context.Background()

	status, err := gitrepo.Status(ctx, repo)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	if _, ok := status["new.txt"]; ok {
		t.Fatalf("Status() reported new.txt before it was created")
	}

	testutil.WriteFile(t, dir, "new.txt", "hello")

	status, err = gitrepo.Status(ctx, repo)
	if err != nil {
		t.Fatalf("Status() after write error = %v", err)
	}
	if got := status["new.txt"]; got == nil || got.Worktree != git.Untracked {
		t.Errorf("Status() without cache did not report new.txt as untracked, got %+v", got)
	}
}

func TestStatus_WithCacheReusesFirstResult(t *testing.T) {
	t.Parallel()

	dir := newRepoWithCommit(t)
	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath(%q) error = %v", dir, err)
	}
	defer repo.Close()

	ctx := gitrepo.WithStatusCache(context.Background())

	if _, err := gitrepo.Status(ctx, repo); err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	// Changing the worktree after the first read is the observable proof that
	// the second call did not recompute: a fresh walk would report new.txt.
	testutil.WriteFile(t, dir, "new.txt", "hello")

	status, err := gitrepo.Status(ctx, repo)
	if err != nil {
		t.Fatalf("Status() second call error = %v", err)
	}
	if _, ok := status["new.txt"]; ok {
		t.Errorf("Status() with cache recomputed instead of reusing the first result")
	}
}

func TestStatus_CacheIsPerWorktree(t *testing.T) {
	t.Parallel()

	dirA := newRepoWithCommit(t)
	dirB := newRepoWithCommit(t)
	testutil.WriteFile(t, dirB, "only-in-b.txt", "hello")

	repoA, err := gitrepo.OpenPath(dirA)
	if err != nil {
		t.Fatalf("OpenPath(%q) error = %v", dirA, err)
	}
	defer repoA.Close()

	repoB, err := gitrepo.OpenPath(dirB)
	if err != nil {
		t.Fatalf("OpenPath(%q) error = %v", dirB, err)
	}
	defer repoB.Close()

	ctx := gitrepo.WithStatusCache(context.Background())

	statusA, err := gitrepo.Status(ctx, repoA)
	if err != nil {
		t.Fatalf("Status(repoA) error = %v", err)
	}
	if _, ok := statusA["only-in-b.txt"]; ok {
		t.Fatalf("Status(repoA) reported a file that only exists in repoB")
	}

	statusB, err := gitrepo.Status(ctx, repoB)
	if err != nil {
		t.Fatalf("Status(repoB) error = %v", err)
	}
	if got := statusB["only-in-b.txt"]; got == nil || got.Worktree != git.Untracked {
		t.Errorf("Status(repoB) reused repoA's entry instead of keying per worktree, got %+v", got)
	}
}

// TestStatus_CachePrunesIgnoredSubtreeRule guards the .gitignore placement that
// keeps go-git from walking e2e/artifacts. go-git only prunes a directory when
// the matching pattern comes from that directory's own parent .gitignore, so a
// nested "outer/ignored/" rule in the root .gitignore does not prune, while
// "ignored/" in outer/.gitignore does. Both must stay ignored either way.
func TestStatus_NestedIgnoreRuleStillIgnores(t *testing.T) {
	t.Parallel()

	dir := newRepoWithCommit(t)
	testutil.WriteFile(t, dir, "outer/.gitignore", "ignored/\n")
	testutil.GitAdd(t, dir, "outer/.gitignore")
	testutil.GitCommit(t, dir, "add nested gitignore")

	if err := os.MkdirAll(filepath.Join(dir, "outer", "ignored", "deep"), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	testutil.WriteFile(t, dir, "outer/ignored/deep/junk.txt", "junk")

	repo, err := gitrepo.OpenPath(dir)
	if err != nil {
		t.Fatalf("OpenPath(%q) error = %v", dir, err)
	}
	defer repo.Close()

	status, err := gitrepo.Status(context.Background(), repo)
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}

	for path := range status {
		if filepath.ToSlash(path) == "outer/ignored/deep/junk.txt" {
			t.Errorf("Status() reported an ignored file: %s", path)
		}
	}
}
