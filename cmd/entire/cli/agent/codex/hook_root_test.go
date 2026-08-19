package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent/testutil"
	"github.com/stretchr/testify/require"
)

func TestResolveHookLocation_NormalCheckout(t *testing.T) {
	t.Parallel()
	repoRoot := filepath.Join(t.TempDir(), "repo")
	initCommittedRepo(t, repoRoot)

	location, err := resolveHookLocation(repoRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, repoRoot), location.HooksPath)
	require.Equal(t, canonicalLockPath(t, repoRoot), location.LockPath)
	require.Empty(t, location.LegacyHooksPath)
	require.False(t, location.RepositoryWide)
}

func TestResolveHookLocation_ConventionalLinkedWorktree(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "repo")
	linkedRoot := filepath.Join(tmp, "linked")
	initCommittedRepo(t, repoRoot)
	runGit(t, repoRoot, "worktree", "add", "-b", "feature", linkedRoot)

	location, err := resolveHookLocation(linkedRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, repoRoot), location.HooksPath)
	require.Equal(t, canonicalHooksPath(t, linkedRoot), location.LegacyHooksPath)
	require.Equal(t, canonicalLockPath(t, repoRoot), location.LockPath)
	require.True(t, location.RepositoryWide)
}

func TestResolveHookLocation_RelativeGitDirAndCommonDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "repo")
	linkedRoot := filepath.Join(tmp, "linked")
	initCommittedRepo(t, repoRoot)
	runGit(t, repoRoot, "worktree", "add", "-b", "feature", linkedRoot)

	gitDir := strings.TrimSpace(strings.TrimPrefix(readFile(t, filepath.Join(linkedRoot, ".git")), "gitdir:"))
	canonicalLinkedRoot, err := canonicalPath(linkedRoot)
	require.NoError(t, err)
	relativeGitDir, err := filepath.Rel(canonicalLinkedRoot, gitDir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(linkedRoot, ".git"), []byte("gitdir: "+relativeGitDir+"\n"), 0o600))
	require.False(t, filepath.IsAbs(strings.TrimSpace(readFile(t, filepath.Join(gitDir, "commondir")))))

	location, err := resolveHookLocation(linkedRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, repoRoot), location.HooksPath)
}

func TestResolveHookLocation_BareWorktreeLayout(t *testing.T) {
	t.Parallel()
	layoutRoot, mainRoot, featureRoot := setupBareWorktreeLayout(t)

	for _, worktreeRoot := range []string{mainRoot, featureRoot} {
		location, err := resolveHookLocation(worktreeRoot)
		require.NoError(t, err)
		require.Equal(t, canonicalHooksPath(t, layoutRoot), location.HooksPath)
		require.Equal(t, canonicalHooksPath(t, worktreeRoot), location.LegacyHooksPath)
		require.True(t, location.RepositoryWide)
	}
}

func TestResolveHookLocation_Submodules(t *testing.T) {
	t.Parallel()
	ordinarySubmoduleRoot, linkedSubmoduleRoot := setupSubmoduleWorktrees(t)

	ordinary, err := resolveHookLocation(ordinarySubmoduleRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, ordinarySubmoduleRoot), ordinary.HooksPath)
	require.False(t, ordinary.RepositoryWide)

	_, err = resolveHookLocation(linkedSubmoduleRoot)
	require.ErrorIs(t, err, ErrLinkedSubmoduleHooksUnsupported)
	require.ErrorContains(t, err, filepath.Join(".git", "modules"))
}

func setupBareWorktreeLayout(t *testing.T) (layoutRoot, mainRoot, featureRoot string) {
	t.Helper()
	tmp := t.TempDir()
	seedRoot := filepath.Join(tmp, "seed")
	layoutRoot = filepath.Join(tmp, "layout")
	bareRoot := filepath.Join(layoutRoot, ".bare")
	mainRoot = filepath.Join(layoutRoot, "main")
	featureRoot = filepath.Join(layoutRoot, "feature")
	initCommittedRepo(t, seedRoot)
	require.NoError(t, os.MkdirAll(layoutRoot, 0o750))
	runGit(t, tmp, "clone", "--bare", seedRoot, bareRoot)
	runGitWithDir(t, tmp, "--git-dir", bareRoot, "worktree", "add", mainRoot)
	runGitWithDir(t, tmp, "--git-dir", bareRoot, "worktree", "add", "-b", "feature", featureRoot)
	return layoutRoot, mainRoot, featureRoot
}

func setupSubmoduleWorktrees(t *testing.T) (ordinarySubmoduleRoot, linkedSubmoduleRoot string) {
	t.Helper()
	tmp := t.TempDir()
	subjectRoot := filepath.Join(tmp, "subject")
	superRoot := filepath.Join(tmp, "super")
	ordinarySubmoduleRoot = filepath.Join(superRoot, "sub")
	linkedSubmoduleRoot = filepath.Join(tmp, "linked-sub")
	initCommittedRepo(t, subjectRoot)
	initCommittedRepo(t, superRoot)
	runGit(t, superRoot, "-c", "protocol.file.allow=always", "submodule", "add", subjectRoot, "sub")
	testutil.GitAdd(t, superRoot, ".gitmodules", "sub")
	testutil.GitCommit(t, superRoot, "add submodule")
	runGit(t, ordinarySubmoduleRoot, "worktree", "add", "-b", "linked", linkedSubmoduleRoot)
	return ordinarySubmoduleRoot, linkedSubmoduleRoot
}

func initCommittedRepo(t *testing.T, repoRoot string) {
	t.Helper()
	testutil.InitRepo(t, repoRoot)
	testutil.WriteFile(t, repoRoot, "README.md", "initial\n")
	testutil.GitAdd(t, repoRoot, "README.md")
	testutil.GitCommit(t, repoRoot, "initial")
}

func runGit(t *testing.T, repoRoot string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", repoRoot}, args...)
	runGitWithDir(t, repoRoot, commandArgs...)
}

func runGitWithDir(t *testing.T, commandDir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = commandDir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func canonicalHooksPath(t *testing.T, root string) string {
	t.Helper()
	canonicalRoot, err := canonicalPath(root)
	require.NoError(t, err)
	return filepath.Join(canonicalRoot, ".codex", HooksFileName)
}

func canonicalLockPath(t *testing.T, root string) string {
	t.Helper()
	canonicalRoot, err := canonicalPath(root)
	require.NoError(t, err)
	return filepath.Join(canonicalRoot, ".git", "entire-codex-hooks.lock")
}
