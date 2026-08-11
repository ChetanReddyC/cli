package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Not parallel: uses t.Chdir()
func TestCheckpointReadRemotes_OriginOnly_NoSetting(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")

	t.Chdir(tmpDir)

	assert.Equal(t, []string{"origin"}, CheckpointReadRemotes(ctx))
}

// Not parallel: uses t.Chdir()
func TestCheckpointReadRemotes_ConfigSettingElectsNonOrigin_OriginAppendsAsLegacyTier(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")
	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")
	testutil.WriteCheckpointPushRemoteSetting(t, tmpDir, "upstream")

	t.Chdir(tmpDir)

	assert.Equal(t, []string{"upstream", "origin"}, CheckpointReadRemotes(ctx))
}

// Not parallel: uses t.Chdir()
func TestCheckpointReadRemotes_SoleRemote_NoOrigin(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "upstream", "https://example.com/upstream.git")

	t.Chdir(tmpDir)

	assert.Equal(t, []string{"upstream"}, CheckpointReadRemotes(ctx))
}

// Not parallel: uses t.Chdir()
func TestCheckpointReadRemotes_MisconfiguredSetting_FailsOpenToOrigin(t *testing.T) {
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

	assert.Equal(t, []string{"origin"}, CheckpointReadRemotes(ctx))
}

// Not parallel: uses t.Chdir()
func TestCheckpointReadRemotes_CorruptSettings_FailsOpenToOrigin(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	testutil.AddRemote(t, tmpDir, "origin", "https://example.com/origin.git")

	// Corrupt settings.json: election fails closed on this, so the read
	// chain must fail OPEN to origin rather than propagate the error.
	entireDir := filepath.Join(tmpDir, ".entire")
	require.NoError(t, os.MkdirAll(entireDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(entireDir, "settings.json"), []byte("{not valid json"), 0o644))

	t.Chdir(tmpDir)

	assert.Equal(t, []string{"origin"}, CheckpointReadRemotes(ctx))
}

// Not parallel: uses t.Chdir()
func TestCheckpointReadRemotes_NoRemotes_Empty(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()
	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)
	testutil.WriteFile(t, tmpDir, "f.txt", "init")
	testutil.GitAdd(t, tmpDir, "f.txt")
	testutil.GitCommit(t, tmpDir, "init")

	t.Chdir(tmpDir)

	assert.Empty(t, CheckpointReadRemotes(ctx))
}

// Not parallel: uses t.Chdir()
// Regression guard: branch tracking config must not influence the read
// chain, mirroring the write-side election it wraps (74e239a9d dropped the
// tracking tier there). Tracking set to "upstream" here must not push
// "upstream" ahead of (or in place of) "origin".
func TestCheckpointReadRemotes_TrackingConfigDoesNotDecide(t *testing.T) {
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

	assert.Equal(t, []string{"origin"}, CheckpointReadRemotes(ctx))
}
