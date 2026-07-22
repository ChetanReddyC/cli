package agentimport

import (
	"regexp"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// shaCandidatePattern enforces the "candidates are SHAs" contract: a hex
// string of plausible short-to-full sha length. Rejects revision syntax like
// "HEAD" or "HEAD~2" from ever reaching ResolveRevision, where it would
// otherwise resolve as a ref/expression rather than a commit sha.
var shaCandidatePattern = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// turnAnchorResolver picks the commit_sha anchor for each imported turn in one
// Run: the LAST candidate (transcript order — the turn's end state) that both
// resolves in the repo and is an ancestor of fallback, else fallback itself.
// fallback is the caller-resolved default-branch tip (Options.LinkCommitSHA);
// ancestry against it doubles as the reachability check, so this needs no
// branch-name logic. Candidates are short SHAs from transcript gitOperation
// records; squash-merged or rebased-away commits simply fail to resolve or
// fail ancestry and fall through, as does any candidate that isn't a
// hex sha (e.g. revision syntax like "HEAD") or that resolves ambiguously.
//
// The fallback's ancestor set is walked and memoized once, lazily, on the
// first turn that actually carries a candidate — a single
// object.NewCommitPreorderIter walk per import run rather than one IsAncestor
// walk per candidate. Turns/sessions with no recorded commits (the common
// case for older transcripts) never trigger the walk. Not safe for concurrent
// use — Run calls resolve from a single goroutine.
type turnAnchorResolver struct {
	repo      *git.Repository
	fallback  string
	ancestors map[plumbing.Hash]struct{} // nil until first candidate-bearing call
}

// newTurnAnchorResolver builds a resolver for one Run. It does no repo work
// until resolve is first called with a non-empty candidate list.
func newTurnAnchorResolver(repo *git.Repository, fallback string) *turnAnchorResolver {
	return &turnAnchorResolver{repo: repo, fallback: fallback}
}

// resolve returns the anchor for one turn's candidates. Empty fallback or no
// candidates return fallback unchanged (empty fallback → "" — unanchorable
// repo imports unlinked, matching resolveImportLinkCommitSHA's contract).
func (r *turnAnchorResolver) resolve(candidates []string) string {
	if r.fallback == "" || len(candidates) == 0 {
		return r.fallback
	}
	if r.ancestors == nil {
		r.ancestors = r.buildAncestors()
	}
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		if !shaCandidatePattern.MatchString(c) {
			continue
		}
		hash, err := r.repo.ResolveRevision(plumbing.Revision(c))
		if err != nil || hash == nil {
			continue
		}
		if _, ok := r.ancestors[*hash]; ok {
			return hash.String()
		}
	}
	return r.fallback
}

// buildAncestors walks fallback's history exactly once, collecting every
// reachable commit hash (including fallback itself — a commit is its own
// ancestor, matching go-git's IsAncestor semantics). If the fallback commit
// doesn't resolve or the history can't be walked, it returns an empty set —
// every candidate then falls through to the fallback in resolve, same as the
// original per-call error handling this replaces.
func (r *turnAnchorResolver) buildAncestors() map[plumbing.Hash]struct{} {
	ancestors := make(map[plumbing.Hash]struct{})
	fbCommit, err := r.repo.CommitObject(plumbing.NewHash(r.fallback))
	if err != nil {
		return ancestors
	}
	iter := object.NewCommitPreorderIter(fbCommit, nil, nil)
	defer iter.Close()
	if err := iter.ForEach(func(c *object.Commit) error {
		ancestors[c.Hash] = struct{}{}
		return nil
	}); err != nil {
		// Best-effort: return whatever was collected before the walk broke
		// (e.g. a missing object deep in history). Any candidate not already
		// in this partial set falls through to the fallback in resolve.
		return ancestors
	}
	return ancestors
}
