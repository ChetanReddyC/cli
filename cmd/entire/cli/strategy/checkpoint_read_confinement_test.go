package strategy

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initRemoteElectionRepo creates an isolated git repository with one seed
// commit for remote-election tests: isolated git config env, InitRepo, and an
// initial commit so HEAD exists. Callers add remotes/settings and t.Chdir
// themselves.
func initRemoteElectionRepo(t *testing.T) string {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	return tmpDir
}

// electionRevParse resolves ref to a hash in dir.
func electionRevParse(t *testing.T, dir, ref string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", ref)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err, "git rev-parse %s", ref)
	return strings.TrimSpace(string(out))
}

// Not parallel: uses t.Chdir()
//
// The 8th resolver pin: a fail-closed election (checkpoint_push_remote names
// a missing remote) with NO origin configured yields an EMPTY chain — the
// fail-open tier only ever substitutes origin, never a non-origin remote such
// as the sole "upstream".
func TestCheckpointReadRemotes_FailClosedElectionNoOrigin_EmptyChain(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

	t.Chdir(tmpDir)

	assert.Empty(t, CheckpointReadRemotes(context.Background()),
		"fail-open must never substitute a non-origin remote")
}

// Not parallel: uses t.Chdir()
//
// #1374-class confinement pin: the election picks upstream, upstream has no
// tracking ref locally, and origin carries a (stale) tracking ref for the
// metadata branch. EnsurePrimaryRef must NOT seed the local primary from
// origin's tracking ref — the legacy read tier never feeds local-ref writes.
func TestEnsurePrimaryRef_StaleOriginTrackingNeverSeedsWhenElectedElsewhere(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "upstream")

	staleHash := electionRevParse(t, tmpDir, "HEAD")
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/origin/"+paths.MetadataBranchName, staleHash)

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err, "an orphan primary ref should have been created")
	assert.NotEqual(t, staleHash, localRef.Hash().String(),
		"local v1 must not be seeded from origin's stale tracking ref when the election picks upstream")
}

// Not parallel: uses t.Chdir()
//
// A fail-closed election (misconfigured checkpoint_push_remote) means there
// is no elected remote: EnsurePrimaryRef must treat this as "nothing to
// advance from" and never fall back to origin's tracking ref.
func TestEnsurePrimaryRef_FailClosedElectionNeverSeedsFromOrigin(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

	staleHash := electionRevParse(t, tmpDir, "HEAD")
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/origin/"+paths.MetadataBranchName, staleHash)

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	assert.NotEqual(t, staleHash, localRef.Hash().String(),
		"a fail-closed election must not seed local v1 from origin")
}

// Not parallel: uses t.Chdir()
//
// Control: with the default election (origin), the legacy seeding behavior is
// preserved byte-for-byte — the local primary is created from origin's
// tracking ref.
func TestEnsurePrimaryRef_ElectedOriginTrackingSeedsLocal(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")

	headHash := electionRevParse(t, tmpDir, "HEAD")
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/origin/"+paths.MetadataBranchName, headHash)

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	assert.Equal(t, headHash, localRef.Hash().String(),
		"the elected remote's tracking ref must seed the local primary")
}

// Not parallel: uses t.Chdir()
//
// The elected remote's tracking ref seeds the local primary even when the
// elected remote is not origin.
func TestEnsurePrimaryRef_ElectedUpstreamTrackingSeedsLocal(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "upstream")

	headHash := electionRevParse(t, tmpDir, "HEAD")
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/upstream/"+paths.MetadataBranchName, headHash)

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	require.NoError(t, EnsurePrimaryRef(ctx, repo))

	localRef, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	assert.Equal(t, headHash, localRef.Hash().String(),
		"the elected upstream tracking ref must seed the local primary")
}

// Not parallel: uses t.Chdir()
//
// GetRemotePrimaryTree is a pure reader over the candidate chain: the elected
// remote's tracking ref wins over origin's when both exist.
func TestGetRemotePrimaryTree_ElectedCandidateWinsOverOrigin(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.WriteFile(t, tmpDir, "second.txt", "second")
	testutil.GitAdd(t, tmpDir, "second.txt")
	testutil.GitCommit(t, tmpDir, "second")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "upstream")

	upstreamHash := electionRevParse(t, tmpDir, "HEAD")
	originHash := electionRevParse(t, tmpDir, "HEAD~1")
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/upstream/"+paths.MetadataBranchName, upstreamHash)
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/origin/"+paths.MetadataBranchName, originHash)

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	tree, err := GetRemotePrimaryTree(ctx, repo)
	require.NoError(t, err)
	_, err = tree.File("second.txt")
	require.NoError(t, err, "the elected upstream tracking ref (which has second.txt) must win over origin's")
}

// Not parallel: uses t.Chdir()
//
// FirstReadCandidateTrackingRef surfaces the elected remote's tracking ref
// first, then origin's, and reports none when neither exists.
func TestFirstReadCandidateTrackingRef_Order(t *testing.T) {
	tmpDir := initRemoteElectionRepo(t)
	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "upstream")

	t.Chdir(tmpDir)
	ctx := context.Background()
	repo, err := OpenRepository(ctx)
	require.NoError(t, err)
	defer repo.Close()

	primary := checkpoint.ResolveRefs(ctx).Primary

	_, _, ok := FirstReadCandidateTrackingRef(ctx, repo, primary)
	assert.False(t, ok, "no tracking ref exists yet")

	headHash := electionRevParse(t, tmpDir, "HEAD")
	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/origin/"+paths.MetadataBranchName, headHash)
	remoteName, refName, ok := FirstReadCandidateTrackingRef(ctx, repo, primary)
	require.True(t, ok)
	assert.Equal(t, "origin", remoteName)
	assert.Equal(t, plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName), refName)

	testutil.GitUpdateRef(t, tmpDir, "refs/remotes/upstream/"+paths.MetadataBranchName, headHash)
	remoteName, refName, ok = FirstReadCandidateTrackingRef(ctx, repo, primary)
	require.True(t, ok)
	assert.Equal(t, "upstream", remoteName, "the elected remote's tracking ref wins when present")
	assert.Equal(t, plumbing.NewRemoteReferenceName("upstream", paths.MetadataBranchName), refName)
}
