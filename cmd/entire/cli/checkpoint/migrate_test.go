package checkpoint

import (
	"context"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/redact"
)

// seedBranchCheckpoint writes one checkpoint to the git-branch v1 store.
func seedBranchCheckpoint(t *testing.T, store *GitStore, cid id.CheckpointID, sessionID string) {
	t.Helper()
	require.NoError(t, store.Write(context.Background(), Session{
		CheckpointID: cid,
		SessionID:    sessionID,
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted([]byte("transcript for " + sessionID + "\n")),
		Prompts:      []string{"do the thing"},
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}))
}

// refHash returns the commit hash a checkpoint's ref points at (fatal if absent).
func refHash(t *testing.T, repo *git.Repository, cid id.CheckpointID) plumbing.Hash {
	t.Helper()
	refName, err := RefName(cid)
	require.NoError(t, err)
	ref, err := repo.Reference(refName, true)
	require.NoError(t, err)
	return ref.Hash()
}

func TestMigrateBranchToRefs(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())

	cid1 := id.MustCheckpointID("a1b2c3d4e5f6")
	cid2 := id.MustCheckpointID("b2c3d4e5f6a1")
	seedBranchCheckpoint(t, branch, cid1, "s1")
	seedBranchCheckpoint(t, branch, cid2, "s2")

	result, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Total)
	assert.Len(t, result.Migrated, 2)
	assert.Equal(t, 0, result.Skipped)

	// Each checkpoint now has a ref whose commit tree IS the branch subtree
	// (byte-identical), and it reads back through the git-refs store.
	branchTree, err := branch.getSessionsBranchTree()
	require.NoError(t, err)
	refsStore := newGitRefsStore(repo)
	for _, cid := range []id.CheckpointID{cid1, cid2} {
		commit, err := repo.CommitObject(refHash(t, repo, cid))
		require.NoError(t, err)

		branchSub, err := refsStore.subtreeObjAt(branchTree.Hash, cid.Path())
		require.NoError(t, err)
		require.NotNil(t, branchSub)
		assert.Equal(t, branchSub.Hash, commit.TreeHash,
			"ref tree must be the branch subtree, byte-identical")

		// A migration commit wraps the tree with no parent (orphan).
		assert.Empty(t, commit.ParentHashes, "first migration commit is an orphan")

		summary, err := refsStore.Read(ctx, cid)
		require.NoError(t, err)
		require.NotNil(t, summary, "migrated checkpoint should read via git-refs")
		assert.Equal(t, cid, summary.CheckpointID)
	}

	// Idempotent: a second run skips everything and leaves the refs untouched.
	before := map[string]plumbing.Hash{cid1.String(): refHash(t, repo, cid1), cid2.String(): refHash(t, repo, cid2)}
	result2, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result2.Total)
	assert.Empty(t, result2.Migrated, "nothing to migrate on a repeat run")
	assert.Equal(t, 2, result2.Skipped)
	assert.Equal(t, before[cid1.String()], refHash(t, repo, cid1), "idempotent re-run must not move refs")
	assert.Equal(t, before[cid2.String()], refHash(t, repo, cid2))
}

func TestMigrateBranchToRefs_AdvancesOnBranchChange(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")

	seedBranchCheckpoint(t, branch, cid, "s1")
	_, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	first := refHash(t, repo, cid)

	// The branch checkpoint gains a second session (its subtree changes).
	seedBranchCheckpoint(t, branch, cid, "s2")

	result, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Len(t, result.Migrated, 1, "changed checkpoint is re-migrated")
	assert.Equal(t, 0, result.Skipped)

	second := refHash(t, repo, cid)
	assert.NotEqual(t, first, second, "ref advances to the new tree")

	// The advance is a fast-forward: the prior migration commit is the parent,
	// so no history is lost.
	commit, err := repo.CommitObject(second)
	require.NoError(t, err)
	require.Len(t, commit.ParentHashes, 1)
	assert.Equal(t, first, commit.ParentHashes[0])
}

func TestMigrateBranchToRefs_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	ctx := context.Background()
	branch := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, branch, cid, "s1")

	result, err := MigrateBranchToRefs(ctx, repo, true)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Total)
	assert.Len(t, result.Migrated, 1, "dry-run reports what would migrate")

	refName, err := RefName(cid)
	require.NoError(t, err)
	_, err = repo.Reference(refName, true)
	assert.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "dry-run must not write refs")
}

func TestMigrateBranchToRefs_NoBranchIsNoop(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t) // initial commit only; no v1 checkpoint branch yet
	result, err := MigrateBranchToRefs(context.Background(), repo, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Total)
	assert.Empty(t, result.Migrated)
}
