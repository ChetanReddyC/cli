package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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
// Each checkpoint's current subtree from the v1 branch tip is wrapped in a
// fresh commit, byte-identical except for the root metadata.json, which is
// normalized for the refs layout (see normalizeMigratedMetadata). Existing
// branch commits are not remapped.
//
// It is idempotent: a ref whose history already contains the normalized tree
// is skipped (even when refs-store writes have advanced the tip past it), and
// a re-run after more branch activity fast-forwards the ref (parenting on the
// existing commit).
//
// New and advanced refs are enqueued for push; this function does not push.
// When dryRun is true it reports what would change without writing refs.
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

		migratedTree, err := migratedCheckpointTree(ctx, repo, cid, cpTreeHash)
		if err != nil {
			return fmt.Errorf("normalize checkpoint %s: %w", cid, err)
		}

		// The existing ref drives the idempotency check and the new commit's
		// parent. Only a ref that resolves to a real commit becomes the parent —
		// unreadable ref state is treated as absent (orphan) rather than
		// parenting the new commit on a bad hash, which would corrupt the commit
		// graph for fetch+replay.
		parent, _, err := refsStore.refBase(cid)
		if err != nil {
			parent = plumbing.ZeroHash
		}
		// Skip when this snapshot was already imported anywhere on the ref's
		// first-parent chain: the ref may have advanced past it through
		// refs-store writes, and re-wrapping the old snapshot would regress
		// the tip.
		if treeInRefHistory(repo, parent, migratedTree) {
			result.Skipped++
			return nil
		}

		if dryRun {
			result.Migrated = append(result.Migrated, cid)
			return nil
		}

		msg := fmt.Sprintf("Import checkpoint %s (migrated from git-branch)", cid)
		commitHash, err := CreateCommit(ctx, repo, migratedTree, parent, msg, authorName, authorEmail)
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

// treeInRefHistory reports whether any commit on the first-parent chain
// starting at tip carries the given tree.
func treeInRefHistory(repo *git.Repository, tip, tree plumbing.Hash) bool {
	for h := tip; h != plumbing.ZeroHash; {
		commit, err := repo.CommitObject(h)
		if err != nil {
			return false
		}
		if commit.TreeHash == tree {
			return true
		}
		if len(commit.ParentHashes) == 0 {
			return false
		}
		h = commit.ParentHashes[0]
	}
	return false
}

// migratedCheckpointTree returns the branch subtree with its root metadata.json
// normalized for the refs layout — unchanged when already normalized or absent.
func migratedCheckpointTree(ctx context.Context, repo *git.Repository, cid id.CheckpointID, cpTreeHash plumbing.Hash) (plumbing.Hash, error) {
	subtree, err := repo.TreeObject(cpTreeHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read checkpoint tree: %w", err)
	}
	metadataFile, err := subtree.File(paths.MetadataFileName)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			return cpTreeHash, nil
		}
		return plumbing.ZeroHash, fmt.Errorf("read metadata.json: %w", err)
	}
	raw, err := metadataFile.Contents()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("read metadata.json: %w", err)
	}

	normalized, changed, err := normalizeMigratedMetadata([]byte(raw), cid)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if !changed {
		return cpTreeHash, nil
	}

	blobHash, err := CreateBlobFromContent(repo, normalized)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("write normalized metadata.json: %w", err)
	}
	newTree, err := ApplyTreeChanges(ctx, repo, cpTreeHash, []TreeChange{{
		Path:  paths.MetadataFileName,
		Entry: &object.TreeEntry{Name: paths.MetadataFileName, Mode: filemode.Regular, Hash: blobHash},
	}})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("build normalized checkpoint tree: %w", err)
	}
	return newTree, nil
}

// normalizeMigratedMetadata rewrites a checkpoint's root metadata.json for the
// refs layout: it drops the legacy checkpoint_version field and strips the
// "/<shard>/<id>" prefix from sessions[] paths. Any session string value under
// the prefix is rebased, so path fields added by other CLI versions are covered
// without naming them. The raw JSON is edited in place so fields this CLI
// doesn't model are preserved. changed is false when the metadata already
// matches the refs layout.
func normalizeMigratedMetadata(raw []byte, cid id.CheckpointID) (normalized []byte, changed bool, err error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("parse metadata.json: %w", err)
	}

	if _, ok := doc["checkpoint_version"]; ok {
		delete(doc, "checkpoint_version")
		changed = true
	}

	branchPrefix := "/" + cid.Path()
	if sessions, ok := doc["sessions"].([]any); ok {
		for _, entry := range sessions {
			session, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			for field, raw := range session {
				value, ok := raw.(string)
				if !ok {
					continue
				}
				if rest, found := strings.CutPrefix(value, branchPrefix); found && strings.HasPrefix(rest, "/") {
					session[field] = rest
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil, false, nil
	}

	normalized, err = jsonutil.MarshalIndentWithNewline(doc, "", "  ")
	if err != nil {
		return nil, false, fmt.Errorf("encode metadata.json: %w", err)
	}
	return normalized, true, nil
}
