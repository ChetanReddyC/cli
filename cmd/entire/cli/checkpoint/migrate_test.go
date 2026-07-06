package checkpoint

import (
	"context"
	"encoding/json"
	"testing"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
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

// mutateBranchCheckpointMetadata rewrites a checkpoint's root metadata.json on
// the v1 branch tip (simulates older-CLI metadata).
func mutateBranchCheckpointMetadata(t *testing.T, repo *git.Repository, cid id.CheckpointID, mutate func(map[string]any)) {
	t.Helper()
	ctx := context.Background()
	branchRef := DefaultV1Refs().Primary
	ref, err := repo.Reference(branchRef, true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)

	metaPath := cid.Path() + "/" + paths.MetadataFileName
	file, err := tree.File(metaPath)
	require.NoError(t, err)
	raw, err := file.Contents()
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	mutate(doc)
	edited, err := json.Marshal(doc)
	require.NoError(t, err)

	blobHash, err := CreateBlobFromContent(repo, edited)
	require.NoError(t, err)
	newTree, err := ApplyTreeChanges(ctx, repo, tree.Hash, []TreeChange{{
		Path:  metaPath,
		Entry: &object.TreeEntry{Name: metaPath, Mode: filemode.Regular, Hash: blobHash},
	}})
	require.NoError(t, err)
	commitHash, err := CreateCommit(ctx, repo, newTree, ref.Hash(), "test: legacy metadata", "Test", "test@test.com")
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(branchRef, commitHash)))
}

// migratedMetadataDoc reads a migrated ref tree's root metadata.json as a JSON object.
func migratedMetadataDoc(t *testing.T, repo *git.Repository, commitTree plumbing.Hash) map[string]any {
	t.Helper()
	tree, err := repo.TreeObject(commitTree)
	require.NoError(t, err)
	file, err := tree.File(paths.MetadataFileName)
	require.NoError(t, err)
	raw, err := file.Contents()
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &doc))
	return doc
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

	// cid1 carries legacy metadata: a checkpoint_version stamp plus an unmodeled field.
	mutateBranchCheckpointMetadata(t, repo, cid1, func(doc map[string]any) {
		doc["checkpoint_version"] = "branch-v1"
		doc["future_field"] = "keep-me"
	})

	result, err := MigrateBranchToRefs(ctx, repo, false)
	require.NoError(t, err)
	assert.Equal(t, 2, result.Total)
	assert.Len(t, result.Migrated, 2)
	assert.Equal(t, 0, result.Skipped)

	// Each ref carries the branch subtree with a normalized root metadata.json
	// and reads back through the git-refs store.
	branchTree, err := branch.getSessionsBranchTree()
	require.NoError(t, err)
	refsStore := newGitRefsStore(repo)
	for _, cid := range []id.CheckpointID{cid1, cid2} {
		commit, err := repo.CommitObject(refHash(t, repo, cid))
		require.NoError(t, err)

		// Session contents carry over byte-identical; only the root
		// metadata.json is rewritten.
		branchSession, err := refsStore.subtreeObjAt(branchTree.Hash, cid.Path()+"/0")
		require.NoError(t, err)
		require.NotNil(t, branchSession)
		refSession, err := refsStore.subtreeObjAt(commit.TreeHash, "0")
		require.NoError(t, err)
		require.NotNil(t, refSession)
		assert.Equal(t, branchSession.Hash, refSession.Hash,
			"session subtree must be the branch's, byte-identical")

		// checkpoint_version is dropped; sessions[] paths are rebased to the ref root.
		doc := migratedMetadataDoc(t, repo, commit.TreeHash)
		assert.NotContains(t, doc, "checkpoint_version", "legacy version stamp must be dropped")
		sessions, ok := doc["sessions"].([]any)
		require.True(t, ok, "sessions must be an array")
		require.Len(t, sessions, 1)
		session, ok := sessions[0].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "/0/metadata.json", session["metadata"])
		assert.Equal(t, "/0/full.jsonl", session["transcript"])
		assert.Equal(t, "/0/prompt.txt", session["prompt"])
		assert.Equal(t, "/0/content_hash.txt", session["content_hash"])

		// A migration commit wraps the tree with no parent (orphan).
		assert.Empty(t, commit.ParentHashes, "first migration commit is an orphan")

		summary, err := refsStore.Read(ctx, cid)
		require.NoError(t, err)
		require.NotNil(t, summary, "migrated checkpoint should read via git-refs")
		assert.Equal(t, cid, summary.CheckpointID)
	}

	// Fields this CLI doesn't model survive the rewrite untouched.
	commit1, err := repo.CommitObject(refHash(t, repo, cid1))
	require.NoError(t, err)
	doc1 := migratedMetadataDoc(t, repo, commit1.TreeHash)
	assert.Equal(t, "keep-me", doc1["future_field"], "unknown metadata fields must be preserved")

	// Migrated refs are enqueued for push (the doctor's "push now" depends on it).
	queue, err := PushQueueForRepo(ctx, repo)
	require.NoError(t, err)
	queued, err := queue.Drain()
	require.NoError(t, err)
	wantQueued := make([]plumbing.ReferenceName, 0, 2)
	for _, cid := range []id.CheckpointID{cid1, cid2} {
		refName, err := RefName(cid)
		require.NoError(t, err)
		wantQueued = append(wantQueued, refName)
	}
	assert.ElementsMatch(t, wantQueued, queued, "migrated refs must be queued for push")

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
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "dry-run must not write refs")

	queue, err := PushQueueForRepo(ctx, repo)
	require.NoError(t, err)
	queued, err := queue.Drain()
	require.NoError(t, err)
	assert.Empty(t, queued, "dry-run must not enqueue refs for push")
}

func TestMigrateBranchToRefs_NoBranchIsNoop(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t) // initial commit only; no v1 checkpoint branch yet
	result, err := MigrateBranchToRefs(context.Background(), repo, false)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Total)
	assert.Empty(t, result.Migrated)
}
