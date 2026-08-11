package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metadataCandidatesFixture builds a local repo with two remotes — "upstream"
// (elected via checkpoint_push_remote) and "origin" (legacy tier) — each a
// local bare repo. The metadata branch is pushed to origin at commit A; when
// alsoUpstream is set it is advanced to commit B and pushed to upstream, so a
// test can prove which candidate served a fetch. The local metadata branch is
// deleted afterwards; the working repo is chdir'd into.
func metadataCandidatesFixture(t *testing.T, alsoUpstream bool) (localDir, originHash, upstreamHash string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)

	tmpDir := t.TempDir()
	originBare := filepath.Join(tmpDir, "origin.git")
	upstreamBare := filepath.Join(tmpDir, "upstream.git")
	localDir = filepath.Join(tmpDir, "local")
	runGit(t, tmpDir, "init", "--bare", originBare)
	runGit(t, tmpDir, "init", "--bare", upstreamBare)

	testutil.InitRepo(t, localDir)
	testutil.WriteFile(t, localDir, "README.md", "hello")
	testutil.GitAdd(t, localDir, "README.md")
	testutil.GitCommit(t, localDir, "init")
	runGit(t, localDir, "remote", "add", "origin", originBare)
	runGit(t, localDir, "remote", "add", "upstream", upstreamBare)

	// Metadata branch at commit A → origin (the legacy tier's copy).
	runGit(t, localDir, "branch", paths.MetadataBranchName)
	runGit(t, localDir, "push", "--quiet", "origin", paths.MetadataBranchName)
	originHash = revParse(t, localDir, paths.MetadataBranchName)

	if alsoUpstream {
		// Advance to commit B → upstream (the elected remote's copy).
		runGit(t, localDir, "checkout", "--quiet", paths.MetadataBranchName)
		testutil.WriteFile(t, localDir, "metadata-b.txt", "checkpoint B")
		testutil.GitAdd(t, localDir, "metadata-b.txt")
		testutil.GitCommit(t, localDir, "checkpoint B")
		runGit(t, localDir, "push", "--quiet", "upstream", paths.MetadataBranchName)
		upstreamHash = revParse(t, localDir, paths.MetadataBranchName)
		runGit(t, localDir, "checkout", "--quiet", "-")
	}

	// Drop local metadata state so the fetch decides what gets created.
	// Tracking refs may or may not exist depending on git's push behavior, so
	// their deletion is best-effort.
	runGit(t, localDir, "branch", "-D", paths.MetadataBranchName)
	for _, ref := range []string{
		"refs/remotes/origin/" + paths.MetadataBranchName,
		"refs/remotes/upstream/" + paths.MetadataBranchName,
	} {
		cmd := exec.CommandContext(t.Context(), "git", "-C", localDir, "update-ref", "-d", ref)
		cmd.Env = testutil.GitIsolatedEnv()
		_ = cmd.Run() //nolint:errcheck // best-effort cleanup of maybe-missing tracking refs
	}

	testutil.WriteCheckpointPushRemoteSetting(t, localDir, "upstream")
	t.Chdir(localDir)
	return localDir, originHash, upstreamHash
}

// refExists reports whether ref resolves in dir.
func refExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "--verify", "--quiet", ref)
	cmd.Env = testutil.GitIsolatedEnv()
	return cmd.Run() == nil
}

// TestFetchMetadataTreeOnly_LegacyOriginFetchNeverAdvancesLocal: the elected
// remote (upstream) has no metadata branch; the legacy origin tier does. The
// fetch must succeed via origin's tracking ref — and must NOT create or
// advance the local metadata branch, which only the elected remote may feed
// (the #1374-class confinement).
//
// Uses t.Chdir — not parallel.
func TestFetchMetadataTreeOnly_LegacyOriginFetchNeverAdvancesLocal(t *testing.T) {
	localDir, originHash, _ := metadataCandidatesFixture(t, false)

	require.NoError(t, FetchMetadataTreeOnly(context.Background()))

	assert.False(t, refExists(t, localDir, "refs/heads/"+paths.MetadataBranchName),
		"a legacy-tier fetch must never create the local metadata branch")
	require.True(t, refExists(t, localDir, "refs/remotes/origin/"+paths.MetadataBranchName),
		"the legacy-tier fetch must land in origin's tracking ref")
	assert.Equal(t, originHash, revParse(t, localDir, "refs/remotes/origin/"+paths.MetadataBranchName))
}

// TestFetchMetadataTreeOnly_ElectedCandidateWinsAndAdvancesLocal: both
// candidates carry the metadata branch at different tips; the elected remote
// (upstream, tried first) must serve the fetch and advance the local branch
// to ITS tip.
//
// Uses t.Chdir — not parallel.
func TestFetchMetadataTreeOnly_ElectedCandidateWinsAndAdvancesLocal(t *testing.T) {
	localDir, originHash, upstreamHash := metadataCandidatesFixture(t, true)

	require.NoError(t, FetchMetadataTreeOnly(context.Background()))

	require.True(t, refExists(t, localDir, "refs/heads/"+paths.MetadataBranchName),
		"the elected remote's fetch must create the local metadata branch")
	got := revParse(t, localDir, "refs/heads/"+paths.MetadataBranchName)
	assert.Equal(t, upstreamHash, got, "the elected candidate must win")
	assert.NotEqual(t, originHash, got)
}

// TestResolveCheckpointPolicyTargets_SplitsReadAndPush: the policy READ path
// iterates the candidate chain with the legacy tier marked read-only, while
// the PUSH target is the elected remote alone.
//
// Uses t.Chdir — not parallel.
func TestResolveCheckpointPolicyTargets_SplitsReadAndPush(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, dir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "upstream")
	t.Chdir(dir)

	readTargets, pushTarget, err := resolveCheckpointPolicyTargets(context.Background())
	require.NoError(t, err)

	require.Len(t, readTargets, 2)
	assert.Equal(t, "upstream", readTargets[0].Remote)
	assert.False(t, readTargets[0].SkipLocalUpdate, "the elected remote may advance the local policy ref")
	assert.Equal(t, "origin", readTargets[1].Remote)
	assert.True(t, readTargets[1].SkipLocalUpdate, "the legacy tier is read-only")

	require.NotNil(t, pushTarget)
	assert.Equal(t, "upstream", pushTarget.Remote, "the push target is the elected remote only")
	assert.False(t, pushTarget.SkipLocalUpdate)
	assert.NotEmpty(t, pushTarget.Dir)
}

// TestResolveCheckpointPolicyTargets_FailClosedElection_NoPushTarget: a
// fail-closed election yields no push target; the fail-open origin remains a
// read-only candidate.
//
// Uses t.Chdir — not parallel.
func TestResolveCheckpointPolicyTargets_FailClosedElection_NoPushTarget(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	testutil.AddRemote(t, dir, "origin", "https://example.com/origin.git")
	testutil.WriteCheckpointPushRemoteSetting(t, dir, "gone")
	t.Chdir(dir)

	readTargets, pushTarget, err := resolveCheckpointPolicyTargets(context.Background())
	require.NoError(t, err)

	require.Len(t, readTargets, 1)
	assert.Equal(t, "origin", readTargets[0].Remote)
	assert.True(t, readTargets[0].SkipLocalUpdate,
		"the fail-open origin candidate must stay read-only — it is not the elected remote")
	assert.Nil(t, pushTarget, "a fail-closed election yields no policy push target")
}
