package strategy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// currentBranchName returns the short name of the current branch in repoDir.
func currentBranchName(t *testing.T, repoDir string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// setGitConfig sets a git config key to value in repoDir.
func setGitConfig(t *testing.T, repoDir, key, value string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "config", key, value)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSetting(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "private", "https://example.com/private.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "private")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "private", Source: SyncRemoteSourceConfig}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSettingMissingRemote_FailsClosed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gone")
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_DefaultsToOrigin(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_SoleRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "upstream", Source: SyncRemoteSourceSole}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_FirstInConfigOrder(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// No "origin" remote. Add zeta before alpha; config-file order should win
	// over alphabetical order.
	testutil.AddRemote(t, tmpDir, "zeta", "https://example.com/zeta.git")
	testutil.AddRemote(t, tmpDir, "alpha", "https://example.com/alpha.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "zeta", Source: SyncRemoteSourceFirst}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_SettingsLoadErrorFailsClosed(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

	// Corrupt settings.json: the file may contain a checkpoint_push_remote
	// we cannot read, so election must not proceed.
	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte("{not valid json"), 0o644))

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot read settings")
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_NoRemotes(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_PushurlOnlyRemoteIsInvisible(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// A remote configured with only a pushurl (no url) added first. If it
	// were counted, it would sort first in .git/config order and get elected.
	cmd := exec.CommandContext(ctx, "git", "config", "remote.pushonly.pushurl", "https://example.com/pushonly.git")
	cmd.Dir = tmpDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())

	// Two real remotes added after it, no "origin" — this keeps the visible
	// remote count at 2 so the resolver exercises the "first" precedence
	// path (not "sole"), proving the pushurl-only entry is excluded from
	// both the count and the ordering.
	testutil.AddRemote(t, tmpDir, "first-real", "https://example.com/first.git")
	testutil.AddRemote(t, tmpDir, "second-real", "https://example.com/second.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "first-real", Source: SyncRemoteSourceFirst}, got)
}

// Not parallel: uses t.Chdir()
// The fork fix: origin is a base repo the user cannot push (cloned base,
// added fork as "upstream", branched with `-u upstream`). Origin must not
// win the election merely by existing — the branch's own tracking config
// takes precedence.
func TestResolveCheckpointSyncRemote_TracksNonOriginRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")

	branch := currentBranchName(t, tmpDir)
	setGitConfig(t, tmpDir, "branch."+branch+".remote", "upstream")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "upstream", Source: SyncRemoteSourceTracking}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_PushRemoteOverridesBranchRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.AddRemote(t, tmpDir, "fork", "https://example.com/fork.git")

	branch := currentBranchName(t, tmpDir)
	setGitConfig(t, tmpDir, "branch."+branch+".remote", "upstream")
	setGitConfig(t, tmpDir, "branch."+branch+".pushRemote", "fork")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "fork", Source: SyncRemoteSourceTracking}, got)
}

// Not parallel: uses t.Chdir()
// Exercises git's full three-way precedence: branch.<name>.pushRemote beats
// remote.pushDefault, which beats branch.<name>.remote.
func TestResolveCheckpointSyncRemote_PushDefaultBeatsBranchRemoteLosesToPushRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.AddRemote(t, tmpDir, "fork", "https://example.com/fork.git")

	branch := currentBranchName(t, tmpDir)
	setGitConfig(t, tmpDir, "branch."+branch+".remote", "upstream")

	// remote.pushDefault beats branch.<name>.remote when pushRemote is unset.
	setGitConfig(t, tmpDir, "remote.pushDefault", "origin")
	t.Chdir(tmpDir)
	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceTracking}, got)

	// branch.<name>.pushRemote beats remote.pushDefault.
	setGitConfig(t, tmpDir, "branch."+branch+".pushRemote", "fork")
	got, err = ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "fork", Source: SyncRemoteSourceTracking}, got)
}

// Not parallel: uses t.Chdir()
// Dangling tracking config (left over from a removed remote) is git state,
// not user intent — it must not fail closed, just fall through to the next
// precedence tier.
func TestResolveCheckpointSyncRemote_DanglingTrackingFallsThroughToOrigin(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")

	branch := currentBranchName(t, tmpDir)
	// "removed" was never added as a remote (simulating a remote that was
	// removed after the branch started tracking it).
	setGitConfig(t, tmpDir, "branch."+branch+".remote", "removed")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_DetachedHEADFallsThroughToOrigin(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")

	head := testutil.GetHeadHash(t, tmpDir)
	checkoutCmd := exec.CommandContext(ctx, "git", "checkout", head)
	checkoutCmd.Dir = tmpDir
	checkoutCmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, checkoutCmd.Run())

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSettingBeatsTracking(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.AddRemote(t, tmpDir, "private", "https://example.com/private.git")

	branch := currentBranchName(t, tmpDir)
	setGitConfig(t, tmpDir, "branch."+branch+".remote", "upstream")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "private")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "private", Source: SyncRemoteSourceConfig}, got)
}

// Not parallel: uses t.Chdir()
func TestCheckpointSyncAllowedForRemote(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	t.Run("no setting: allowed only for the elected default remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

		t.Chdir(tmpDir)

		assert.True(t, checkpointSyncAllowedForRemote(ctx, "origin"))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish"))
	})

	t.Run("misconfigured setting fails closed for every remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")
		testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "gone")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin"))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish"))
	})

	t.Run("unreadable settings fails closed for every remote", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
		testutil.AddRemote(t, tmpDir, "publish", "https://example.com/publish.git")

		// Corrupt settings.json, not a misconfigured setting: the gate must
		// fail closed here too, not just when the resolver itself detects a
		// bad checkpoint_push_remote value.
		entireDir := filepath.Join(tmpDir, ".entire")
		require.NoError(t, os.MkdirAll(entireDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte("{not valid json"), 0o644))

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin"))
		assert.False(t, checkpointSyncAllowedForRemote(ctx, "publish"))
	})

	t.Run("raw URL push argument is never allowed", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "https://github.com/o/r.git"))
	})

	t.Run("no remotes configured: never allowed", func(t *testing.T) {
		tmpDir := t.TempDir()
		testutil.InitRepo(t, tmpDir)
		testutil.WriteFile(t, tmpDir, "f.txt", "init")
		testutil.GitAdd(t, tmpDir, "f.txt")
		testutil.GitCommit(t, tmpDir, "init")

		t.Chdir(tmpDir)

		assert.False(t, checkpointSyncAllowedForRemote(ctx, "origin"))
	})
}
