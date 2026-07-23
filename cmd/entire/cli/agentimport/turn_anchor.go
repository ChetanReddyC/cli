package agentimport

import (
	"context"
	"regexp"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// shaCandidatePattern enforces the "candidates are SHAs" contract: a hex
// string of plausible short-to-full sha length. Rejects revision syntax like
// "HEAD" or "HEAD~2" from ever reaching ResolveRevision, where it would
// otherwise resolve as a ref/expression rather than a commit sha. It does NOT
// stop a hex-named ref: a branch or tag literally named e.g. "beef" still
// resolves as that ref before a commit sha would. That's accepted here — the
// ancestry gate below still bounds the result to default-branch history, and
// this anchor is display-only.
var shaCandidatePattern = regexp.MustCompile(`^[0-9a-f]{4,64}$`)

// turnAnchorResolver picks the commit_sha anchor for each imported turn in one
// Run: the LAST candidate (transcript order — the turn's end state) that both
// resolves in the repo and is an ancestor of fallback, else fallback itself.
// fallback is the caller-resolved default-branch tip (Options.LinkCommitSHA);
// ancestry against it doubles as the reachability check, so this needs no
// branch-name logic. Candidates are abbreviated commit SHAs recorded by the
// turn's transcript; squash-merged or rebased-away commits simply fail to
// resolve or fail ancestry and fall through, as does any candidate that isn't
// a hex sha (e.g. revision syntax like "HEAD"). Ambiguous short SHAs are NOT
// detected — go-git's ResolveRevision resolves them to an arbitrary matching
// commit rather than erroring; the ancestry gate bounds the resulting damage
// to mis-anchoring within default-branch history, never outside it.
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
// until resolve is first called with a non-empty candidate list. fallback
// must be a full hex sha when non-empty — resolveImportLinkCommitSHA
// guarantees this; a short fallback would silently degrade to the empty
// ancestor-set path (CommitObject requires a full hash).
func newTurnAnchorResolver(repo *git.Repository, fallback string) *turnAnchorResolver {
	return &turnAnchorResolver{repo: repo, fallback: fallback}
}

// resolve returns the anchor for one turn's candidates. Empty fallback or no
// candidates return fallback unchanged (empty fallback → "" — unanchorable
// repo imports unlinked, matching resolveImportLinkCommitSHA's contract).
func (r *turnAnchorResolver) resolve(ctx context.Context, candidates []string) string {
	if r.fallback == "" || len(candidates) == 0 {
		return r.fallback
	}
	if r.ancestors == nil {
		r.ancestors = r.buildAncestors(ctx)
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
// doesn't resolve or the history can't be walked, it returns an empty (or
// partial) set — every candidate not already collected then falls through to
// the fallback in resolve. Both failure paths are logged at Debug: import
// decisions are one-shot (a re-run skips already-imported turns), so an
// unlogged failure here destroys the only evidence of why every turn in this
// run anchored to the fallback instead of its recorded commit.
func (r *turnAnchorResolver) buildAncestors(ctx context.Context) map[plumbing.Hash]struct{} {
	ancestors := make(map[plumbing.Hash]struct{})
	fbCommit, err := r.repo.CommitObject(plumbing.NewHash(r.fallback))
	if err != nil {
		logging.Debug(ctx, "import: anchor ancestor walk unavailable, all turns fall back",
			"fallback", r.fallback, "error", err.Error())
		return ancestors
	}
	iter := object.NewCommitPreorderIter(fbCommit, nil, nil)
	defer iter.Close()
	if err := iter.ForEach(func(c *object.Commit) error {
		ancestors[c.Hash] = struct{}{}
		return nil
	}); err != nil {
		logging.Debug(ctx, "import: anchor ancestor walk truncated",
			"fallback", r.fallback, "ancestors_collected", len(ancestors), "error", err.Error())
		return ancestors
	}
	return ancestors
}
