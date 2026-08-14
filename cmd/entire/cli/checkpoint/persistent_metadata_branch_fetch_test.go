package checkpoint

import (
	"context"
	"errors"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
)

// dropV1Branch removes the local v1 ref after capturing its hash, modelling a
// fresh clone: the checkpoints exist on the checkpoint remote, but no local ref
// points at them and origin never carried the branch either.
func dropV1Branch(t *testing.T, repo *git.Repository) plumbing.Hash {
	t.Helper()
	primary := DefaultV1Refs().Primary
	ref, err := repo.Reference(primary, true)
	require.NoError(t, err)
	hash := ref.Hash()
	require.NoError(t, repo.Storer.RemoveReference(primary))
	return hash
}

// TestGetSessionsBranchTree_FetchesMetadataBranchWhenMissing pins the recovery
// tier that makes a dedicated checkpoint_remote readable from a fresh clone.
//
// The git-branch store previously resolved reads from the local ref, then from
// origin's remote-tracking ref, and gave up. A repo whose checkpoints live on a
// separate checkpoint_remote has neither — origin never received the branch — so
// every committed checkpoint read as "not found" with no path to recovery.
func TestGetSessionsBranchTree_FetchesMetadataBranchWhenMissing(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cid := id.MustCheckpointID("a1b2c3d4e5f6")
	seedBranchCheckpoint(t, store, cid, "s1")

	hash := dropV1Branch(t, repo)

	// Without a fetcher the read still fails, exactly as before.
	_, err := store.getSessionsBranchTree(t.Context())
	require.Error(t, err, "a missing branch with no fetcher must still fail")

	calls := 0
	store.SetMetadataBranchFetcher(func(_ context.Context) error {
		calls++
		return repo.Storer.SetReference(plumbing.NewHashReference(DefaultV1Refs().Primary, hash))
	})

	tree, err := store.getSessionsBranchTree(t.Context())
	require.NoError(t, err, "the fetcher should have made the branch readable")
	assert.Equal(t, 1, calls, "fetcher should be called exactly once")

	// The recovered tree is the real one: it carries the seeded checkpoint.
	_, err = tree.Tree(cid.Path())
	require.NoError(t, err, "recovered tree should contain the seeded checkpoint")
}

// TestGetSessionsBranchTree_SkipsFetchWhenBranchPresent pins that the fetcher is
// a recovery tier, not a refresh: a branch that resolves locally must never
// trigger a network call. Reads run on hot paths where an unconditional fetch
// would be a per-read stall.
func TestGetSessionsBranchTree_SkipsFetchWhenBranchPresent(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	seedBranchCheckpoint(t, store, id.MustCheckpointID("a1b2c3d4e5f6"), "s1")

	store.SetMetadataBranchFetcher(func(_ context.Context) error {
		t.Error("fetcher must not run when the branch resolves locally")
		return nil
	})

	_, err := store.getSessionsBranchTree(t.Context())
	require.NoError(t, err)
}

// TestGetSessionsBranchTree_FetchFailureKeepsOriginalError pins that a failing
// fetch degrades to the pre-existing "not found" error rather than replacing it
// with a transport error. Callers such as List treat not-found as an empty
// result, so surfacing the fetch failure here would turn an offline read into a
// hard error.
func TestGetSessionsBranchTree_FetchFailureKeepsOriginalError(t *testing.T) {
	t.Parallel()
	repo, _ := setupBranchTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	seedBranchCheckpoint(t, store, id.MustCheckpointID("a1b2c3d4e5f6"), "s1")
	dropV1Branch(t, repo)

	_, wantErr := store.getSessionsBranchTree(t.Context())
	require.Error(t, wantErr)

	store.SetMetadataBranchFetcher(func(_ context.Context) error {
		return errors.New("no checkpoint_remote configured")
	})

	_, err := store.getSessionsBranchTree(t.Context())
	require.Error(t, err)
	assert.Equal(t, wantErr.Error(), err.Error(),
		"a failed fetch should leave the original not-found error intact")
}
