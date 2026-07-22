package agentimport

import (
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// buildAnchorTestRepo builds:
//
//	main:   C1 ── C2   (fallback anchor = C2's full sha)
//	side:   C1 ── S1   (side branch commit — resolvable but NOT an ancestor of C2)
//
// and returns the repo plus the full SHAs of C1, C2, and S1. turnAnchorResolver
// never consults HEAD/current branch, so the repo is left checked out on the
// side branch after this helper runs — that's fine for these tests.
func buildAnchorTestRepo(t *testing.T) (repo *git.Repository, c1, c2, s1 string) {
	t.Helper()
	repo, repoDir := initRepoWithCommit(t)
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c1 = head.Hash().String()

	// C2 on the default branch.
	writeAndCommit(t, wt, repoDir, "f.txt", "c2", "second")
	head, err = repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	c2 = head.Hash().String()

	// side branch off C1, with a commit S1 that the default branch never merges.
	if err := wt.Checkout(&git.CheckoutOptions{
		Hash:   plumbing.NewHash(c1),
		Branch: plumbing.NewBranchReferenceName("side"),
		Create: true,
	}); err != nil {
		t.Fatal(err)
	}
	writeAndCommit(t, wt, repoDir, "f.txt", "s1", "side commit")
	head, err = repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	s1 = head.Hash().String()

	return repo, c1, c2, s1
}

func writeAndCommit(t *testing.T, wt *git.Worktree, repoDir, file, content, msg string) {
	t.Helper()
	testutil.WriteFile(t, repoDir, file, content)
	if _, err := wt.Add(file); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@test.com"},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResolveTurnAnchor_PicksLastReachableCandidate(t *testing.T) {
	t.Parallel()
	repo, c1, c2, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2)

	got := r.resolve([]string{c1[:7], c2[:7]})
	if got != c2 {
		t.Fatalf("resolve = %q, want last candidate (full) %q", got, c2)
	}
}

func TestResolveTurnAnchor_SkipsUnreachableAndUnresolvable(t *testing.T) {
	t.Parallel()
	repo, _, c2, s1 := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2)

	// s1 resolves but is not an ancestor of the fallback c2.
	if got := r.resolve([]string{s1[:7]}); got != c2 {
		t.Fatalf("unreachable candidate: resolve = %q, want fallback %q", got, c2)
	}

	// "deadbeef" is valid hex but doesn't resolve to anything in this repo.
	if got := r.resolve([]string{"deadbeef"}); got != c2 {
		t.Fatalf("unresolvable candidate: resolve = %q, want fallback %q", got, c2)
	}

	// nil candidates.
	if got := r.resolve(nil); got != c2 {
		t.Fatalf("nil candidates: resolve = %q, want fallback %q", got, c2)
	}
}

// TestResolveTurnAnchor_RejectsRevisionSyntax proves a candidate that looks
// like git revision syntax rather than a sha (e.g. "HEAD") is rejected before
// ever reaching ResolveRevision, so it can't resolve as an expression and
// falls through to the fallback like any other unresolvable candidate.
func TestResolveTurnAnchor_RejectsRevisionSyntax(t *testing.T) {
	t.Parallel()
	repo, _, c2, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, c2)

	if got := r.resolve([]string{"HEAD"}); got != c2 {
		t.Fatalf("revision syntax candidate: resolve = %q, want fallback %q", got, c2)
	}
	if got := r.resolve([]string{"HEAD~2"}); got != c2 {
		t.Fatalf("revision syntax candidate: resolve = %q, want fallback %q", got, c2)
	}
}

func TestResolveTurnAnchor_EmptyFallback(t *testing.T) {
	t.Parallel()
	repo, c1, _, _ := buildAnchorTestRepo(t)
	r := newTurnAnchorResolver(repo, "")

	if got := r.resolve([]string{c1[:7]}); got != "" {
		t.Fatalf("empty fallback: resolve = %q, want empty", got)
	}
}
