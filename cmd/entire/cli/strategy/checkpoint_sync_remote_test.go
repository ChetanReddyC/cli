package strategy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addRemote adds a git remote named name pointing at url in repoDir.
func addRemote(t *testing.T, repoDir, name, url string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", name, url)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())
}

// writeCheckpointPushRemoteSetting writes .entire/settings.json configuring
// strategy_options.checkpoint_push_remote to remoteName.
func writeCheckpointPushRemoteSetting(t *testing.T, repoDir, remoteName string) {
	t.Helper()
	entireDir := filepath.Join(repoDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	content := `{"enabled": true, "strategy_options": {"checkpoint_push_remote": "` + remoteName + `"}}`
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(content), 0o644))
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSetting(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	addRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	addRemote(t, tmpDir, "private", "https://example.com/private.git")
	writeCheckpointPushRemoteSetting(t, tmpDir, "private")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "private", Source: SyncRemoteSourceConfig}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_ConfigSettingMissingRemote_FailsClosed(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	addRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	writeCheckpointPushRemoteSetting(t, tmpDir, "gone")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gone")
	assert.Empty(t, got.Name)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_DefaultsToOrigin(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	addRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	addRemote(t, tmpDir, "publish", "https://example.com/publish.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "origin", Source: SyncRemoteSourceDefault}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_SoleRemote(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	addRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "upstream", Source: SyncRemoteSourceSole}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_FirstInConfigOrder(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	// No "origin" remote. Add zeta before alpha; config-file order should win
	// over alphabetical order.
	addRemote(t, tmpDir, "zeta", "https://example.com/zeta.git")
	addRemote(t, tmpDir, "alpha", "https://example.com/alpha.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "zeta", Source: SyncRemoteSourceFirst}, got)
}

// Not parallel: uses t.Chdir()
func TestResolveCheckpointSyncRemote_NoRemotes(t *testing.T) {
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
	addRemote(t, tmpDir, "first-real", "https://example.com/first.git")
	addRemote(t, tmpDir, "second-real", "https://example.com/second.git")

	t.Chdir(tmpDir)

	got, err := ResolveCheckpointSyncRemote(ctx)
	require.NoError(t, err)
	assert.Equal(t, CheckpointSyncRemote{Name: "first-real", Source: SyncRemoteSourceFirst}, got)
}
