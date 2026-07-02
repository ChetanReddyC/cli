package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// MigrateResult summarizes a git-branch → git-refs checkpoint migration.
type MigrateResult struct {
	// Total is the number of checkpoints found on the v1 branch.
	Total int
	// Migrated lists the checkpoints whose ref was newly written or advanced.
	Migrated []id.CheckpointID
	// Skipped counts checkpoints already up to date (idempotent no-ops).
	Skipped int
}

// MigrateBranchToRefs converts every checkpoint stored on the git-branch v1
// branch (entire/checkpoints/v1) into a per-checkpoint ref under
// refs/entire/checkpoints/<shard>/<id> — the layout the git-refs store uses.
//
// For each checkpoint it takes the checkpoint's CURRENT subtree object from the
// v1 branch tip and wraps it in a fresh commit, then points the ref at that
// commit. Existing branch commits are not remapped — only the latest tree is
// carried over. Because the git-refs ref tree is byte-for-byte the branch's
// <shard>/<id> subtree, a migrated checkpoint reads identically under either
// backend.
//
// It is idempotent: a checkpoint whose ref already points at a commit carrying
// the same tree is skipped, so re-running after more branch activity converts
// only what changed — the new commit parents on the existing ref (a
// fast-forward) rather than orphaning, so no prior state is lost from history.
//
// New and advanced refs are enqueued for push like any git-refs write; this
// function does not push. When dryRun is true it reports what would change
// (populating Migrated) without writing any refs.
func MigrateBranchToRefs(ctx context.Context, repo *git.Repository, dryRun bool) (MigrateResult, error) {
	var result MigrateResult

	branch := NewGitStore(repo, DefaultV1Refs())
	tree, err := branch.getSessionsBranchTree()
	if err != nil {
		if errors.Is(err, plumbing.ErrReferenceNotFound) {
			// No v1 branch locally or on origin → nothing to migrate.
			return result, nil
		}
		return result, fmt.Errorf("read v1 checkpoint branch: %w", err)
	}

	refsStore := newGitRefsStore(repo)
	authorName, authorEmail := GetGitAuthorFromRepo(repo)

	walkErr := WalkCheckpointShards(ctx, repo, tree, func(cid id.CheckpointID, cpTreeHash plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // propagate context cancellation
		}
		result.Total++

		refName, err := RefName(cid)
		if err != nil {
			// A malformed id on the branch can't map to a ref; skip it rather
			// than aborting the whole migration.
			logging.Warn(ctx, "migrate: skipping checkpoint with unmappable id",
				slog.String("id", cid.String()), slog.String("error", err.Error()))
			return nil
		}

		// Resolve the existing ref once: it drives both the idempotency check and
		// the parent of the new commit.
		parent := plumbing.ZeroHash
		if existing, err := repo.Reference(refName, true); err == nil {
			parent = existing.Hash()
			if commit, cerr := repo.CommitObject(parent); cerr == nil && commit.TreeHash == cpTreeHash {
				result.Skipped++
				return nil
			}
		}

		if dryRun {
			result.Migrated = append(result.Migrated, cid)
			return nil
		}

		// Wrap the checkpoint's current subtree in a fresh commit — parenting on
		// the existing ref when present (re-migration fast-forwards) or as an
		// orphan for a brand-new ref — then point the ref at it and enqueue it.
		msg := fmt.Sprintf("Import checkpoint %s (migrated from git-branch)", cid)
		commitHash, err := CreateCommit(ctx, repo, cpTreeHash, parent, msg, authorName, authorEmail)
		if err != nil {
			return fmt.Errorf("commit checkpoint %s: %w", cid, err)
		}
		if err := refsStore.setRef(ctx, cid, commitHash); err != nil {
			return fmt.Errorf("set ref for checkpoint %s: %w", cid, err)
		}
		result.Migrated = append(result.Migrated, cid)
		return nil
	})
	if walkErr != nil {
		return result, fmt.Errorf("walk v1 checkpoints: %w", walkErr)
	}
	return result, nil
}
