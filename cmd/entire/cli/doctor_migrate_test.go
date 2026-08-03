package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// addMigrateTestRemote adds a git remote named name pointing at url in repoDir.
func addMigrateTestRemote(t *testing.T, repoDir, name, url string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "remote", "add", name, url)
	cmd.Dir = repoDir
	cmd.Env = testutil.GitIsolatedEnv()
	require.NoError(t, cmd.Run())
}

// writeMigrateTestCheckpointPushRemoteSetting writes .entire/settings.json
// configuring strategy_options.checkpoint_push_remote to remoteName.
func writeMigrateTestCheckpointPushRemoteSetting(t *testing.T, repoDir, remoteName string) {
	t.Helper()
	entireDir := filepath.Join(repoDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	content := `{"enabled": true, "strategy_options": {"checkpoint_push_remote": "` + remoteName + `"}}`
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte(content), 0o644))
}

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_ExplicitValueReturnedVerbatim(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "explicit-remote")
	require.NoError(t, err)
	assert.Equal(t, "explicit-remote", got)
}

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_EmptyUsesConfiguredSetting(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	addMigrateTestRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	addMigrateTestRemote(t, tmpDir, "private", "https://example.com/private.git")
	writeMigrateTestCheckpointPushRemoteSetting(t, tmpDir, "private")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "private", got)
}

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_EmptyDefaultsToOrigin(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	addMigrateTestRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	addMigrateTestRemote(t, tmpDir, "publish", "https://example.com/publish.git")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "origin", got)
}

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_MisconfiguredSettingFailsClosed(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	addMigrateTestRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	writeMigrateTestCheckpointPushRemoteSetting(t, tmpDir, "gone")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checkpoint_push_remote")
	assert.Empty(t, got)
}

// Not parallel: uses t.Chdir()
func TestResolveMigratePushRemote_EmptyNoRemotesErrors(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	t.Chdir(tmpDir)

	got, err := resolveMigratePushRemote(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--remote")
	assert.Empty(t, got)
}

func TestDoctorMigrateCheckpoints_RefusesWhenRefsPrimary(t *testing.T) {
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".entire"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".entire", "settings.json"),
		[]byte(`{"enabled": true, "checkpoints": {"primary": {"type": "git-refs"}}}`), 0o644))
	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()

	cmd := newDoctorMigrateCheckpointsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "already the primary",
		"must refuse to migrate when git-refs is already the primary store")
}
