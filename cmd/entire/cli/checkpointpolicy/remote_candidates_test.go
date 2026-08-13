package checkpointpolicy_test

import (
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpointpolicy"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

func initPolicyBare(t *testing.T, policy checkpointpolicy.Policy) (string, plumbing.Hash) {
	t.Helper()
	workDir, workRepo := initPolicyRepoWithDir(t)
	hash, err := checkpointpolicy.WriteLocal(t.Context(), workRepo, plumbing.ZeroHash, policy)
	require.NoError(t, err)
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	_, err = git.PlainInit(bareDir, true)
	require.NoError(t, err)
	pushPolicyRefWithGit(t, workDir, bareDir)
	return bareDir, hash
}

func TestSyncFromPolicyFirstCandidateWins(t *testing.T) {
	firstBare, firstHash := initPolicyBare(t, checkpointpolicy.DefaultPolicy())
	secondBare, secondHash := initPolicyBare(t, checkpointpolicy.Policy{
		CheckpointVersion:    "refs-v2",
		CheckpointMinVersion: "refs-v2",
	})
	require.NotEqual(t, firstHash, secondHash)

	localDir, localRepo := initPolicyRepoWithDir(t)
	got, err := checkpointpolicy.SyncFrom(t.Context(), localRepo, []checkpointpolicy.Target{
		{Remote: firstBare, Dir: localDir},
		{Remote: secondBare, Dir: localDir},
	})
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceRemote, got.Source)
	require.Equal(t, firstHash, got.Hash, "the first candidate's policy must win")
}

func TestSyncFromPolicyTransportErrorAdvances(t *testing.T) {
	secondBare, secondHash := initPolicyBare(t, checkpointpolicy.DefaultPolicy())

	localDir, localRepo := initPolicyRepoWithDir(t)
	got, err := checkpointpolicy.SyncFrom(t.Context(), localRepo, []checkpointpolicy.Target{
		{Remote: filepath.Join(localDir, "nonexistent-remote"), Dir: localDir},
		{Remote: secondBare, Dir: localDir},
	})
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceRemote, got.Source)
	require.Equal(t, secondHash, got.Hash)
}

func TestSyncFromPolicyAllFailSurfacesFirstError(t *testing.T) {
	localDir, localRepo := initPolicyRepoWithDir(t)
	_, err := checkpointpolicy.SyncFrom(t.Context(), localRepo, []checkpointpolicy.Target{
		{Remote: filepath.Join(localDir, "missing-one"), Dir: localDir},
		{Remote: filepath.Join(localDir, "missing-two"), Dir: localDir},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "check remote checkpoint policy ref")
	require.Contains(t, err.Error(), "missing-one",
		"the first candidate's error must be the one surfaced")
	require.NotContains(t, err.Error(), "missing-two",
		"the second candidate's error must not mask the first's")
}

func TestSyncFromPolicySkipLocalUpdateNeverAdvancesLocalRef(t *testing.T) {
	bareDir, remoteHash := initPolicyBare(t, checkpointpolicy.DefaultPolicy())

	localDir, localRepo := initPolicyRepoWithDir(t)
	got, err := checkpointpolicy.SyncFrom(t.Context(), localRepo, []checkpointpolicy.Target{
		{Remote: bareDir, Dir: localDir, SkipLocalUpdate: true},
	})
	require.NoError(t, err)
	require.Equal(t, checkpointpolicy.SourceRemote, got.Source, "the baseline is still reported")
	require.Equal(t, remoteHash, got.Hash)

	localState, err := checkpointpolicy.ReadLocal(t.Context(), localRepo)
	require.NoError(t, err)
	require.True(t, localState.Hash.IsZero(),
		"a legacy-tier baseline must never advance the local policy ref")
}
