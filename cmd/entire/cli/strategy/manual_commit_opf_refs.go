// Pre-push OPF rewrite for the git-refs checkpoint backend, the sibling of
// manual_commit_opf_rewrite.go's entire/checkpoints/v1 rewrite. Both run the
// OPF-augmented redaction once per push; only discovery and ref update differ.
package strategy

import (
	"context"
	"errors"
	"fmt"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/redact"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// RewriteQueuedCheckpointRefsWithOPF re-redacts the checkpoint refs awaiting
// push with OPF, rebuilds each as a commit carrying Entire-OPF-Applied: true,
// and points its ref at the new commit. Idempotent: a ref whose commit already
// carries the trailer is left byte-identical.
//
// Discovery is the push-discovery queue rather than a local-vs-remote diff: it
// already names exactly the refs this push will carry, so there is no
// merge-base, divergence, or bootstrap analysis. Each checkpoint ref is
// standalone, so a rebuilt commit keeps its original parent instead of being
// re-parented onto a rewritten chain.
//
// Caller checks redact.OPFEnabled() and skips this when OPF is off. Returns
// the same error taxonomy as RewriteUnpushedV1WithOPF; the caller fails closed
// by withholding the flush (see prePushCheckpointRefs).
func RewriteQueuedCheckpointRefsWithOPF(ctx context.Context, repo *git.Repository) error {
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return fmt.Errorf("resolve push queue: %w", err)
	}
	// Peek, not Drain: flushCheckpointRefsQueue owns draining and pruning.
	queued, err := queue.Peek()
	if err != nil {
		return fmt.Errorf("peek push queue: %w", err)
	}
	if len(queued) == 0 {
		return nil
	}

	// Both up-front fail-closed gates, in the same order as the v1 rewrite, so
	// a misconfigured category set surfaces config remediation rather than
	// "verify your OPF install".
	if redact.OPFMisconfiguredNoCategories() {
		return &OPFNoCategoriesError{}
	}
	if redact.OPFBreakerTripped() {
		return &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand()}
	}

	// Pass 1: collect every redactable blob from every queued ref still needing
	// OPF, bounding raw bytes in memory exactly as the v1 collect pass does.
	type pendingRef struct {
		ref    plumbing.ReferenceName
		old    plumbing.Hash
		commit *object.Commit
		// blobs and paths are parallel; startIdx is this ref's offset into the
		// global redacted slice.
		blobs    []redact.NamedBlob
		paths    []string
		startIdx int
	}
	var globalBlobs []redact.NamedBlob
	pendings := make([]pendingRef, 0, len(queued))
	rawCap := scaleBatchLimit(resolveBatchLimit(), rawByteCapMultiplier)
	var rawBytesSoFar int
	// Stale entries (refs no longer present locally) are skipped, not pruned:
	// the queue belongs to the flush.
	existing, _ := partitionLocalRefs(repo, queued)
	for _, refName := range existing {
		ref, refErr := repo.Reference(refName, true)
		if refErr != nil {
			if errors.Is(refErr, plumbing.ErrReferenceNotFound) {
				continue
			}
			return fmt.Errorf("resolve checkpoint ref %s: %w", refName, refErr)
		}
		c, commitErr := repo.CommitObject(ref.Hash())
		if commitErr != nil {
			return fmt.Errorf("load checkpoint commit for %s: %w", refName, commitErr)
		}
		if trailers.HasOPFApplied(c.Message) {
			continue
		}
		tree, treeErr := repo.TreeObject(c.TreeHash)
		if treeErr != nil {
			return fmt.Errorf("load tree for %s: %w", refName, treeErr)
		}
		pr := pendingRef{ref: refName, old: ref.Hash(), commit: c, startIdx: len(globalBlobs)}
		if err := collectTreeBlobs(repo, tree, "", &pr.blobs, &pr.paths); err != nil {
			return fmt.Errorf("collect blobs %s: %w", refName, err)
		}
		for _, b := range pr.blobs {
			rawBytesSoFar += len(b.Content)
		}
		if rawBytesSoFar > rawCap {
			return &OPFRawBytesTooLargeError{RawBytes: rawBytesSoFar, Limit: rawCap}
		}
		globalBlobs = append(globalBlobs, pr.blobs...)
		pendings = append(pendings, pr)
	}
	if len(pendings) == 0 {
		return nil
	}

	// Pass 2: enforce the leaf-byte cap, then make exactly ONE OPF shell-out
	// for the whole flush.
	var globalRedacted [][]byte
	if len(globalBlobs) > 0 {
		leafBytes := redact.SumProseLeafBytes(globalBlobs)
		if limit := resolveBatchLimit(); leafBytes > limit {
			return &OPFBatchTooLargeError{LeafBytes: leafBytes, Limit: limit}
		}
		globalRedacted, err = redact.BatchBytesWithPrivacyFilter(ctx, globalBlobs)
		if err != nil {
			if errors.Is(err, redact.ErrOPFNoEnabledCategories) {
				return &OPFNoCategoriesError{}
			}
			return &OPFRuntimeFailedError{OPFCommand: redact.OPFCommand(), Cause: err}
		}
	}

	// Pass 3: rebuild every commit before touching any ref, so a failure
	// part-way through leaves every ref where it was.
	rebuilt := make([]plumbing.Hash, len(pendings))
	for i, pr := range pendings {
		redactedByPath := make(map[string][]byte, len(pr.blobs))
		for j, path := range pr.paths {
			redactedByPath[path] = globalRedacted[pr.startIdx+j]
		}
		var parent plumbing.Hash
		if len(pr.commit.ParentHashes) > 0 {
			parent = pr.commit.ParentHashes[0]
		}
		newHash, rebuildErr := rebuildCheckpointCommit(ctx, repo, pr.commit, parent, redactedByPath)
		if rebuildErr != nil {
			return fmt.Errorf("rebuild checkpoint commit %s: %w", pr.ref, rebuildErr)
		}
		rebuilt[i] = newHash
	}

	// CAS each ref: a concurrent write that advanced a checkpoint ref during
	// the rewrite must not be clobbered by our stale rebuild.
	for i, pr := range pendings {
		if err := repo.Storer.CheckAndSetReference(
			plumbing.NewHashReference(pr.ref, rebuilt[i]),
			plumbing.NewHashReference(pr.ref, pr.old),
		); err != nil {
			return fmt.Errorf("update checkpoint ref %s: %w", pr.ref, err)
		}
	}
	return nil
}
