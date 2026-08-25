package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestResolveHookDiscovery_NormalCheckout(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "repo")
	initCommittedRepo(t, root)

	discovery := resolveHookDiscovery(root)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, root), discovery.DiscoveredHooks.Path())
	require.False(t, discovery.RepositoryWide)
	require.NoError(t, discovery.Diagnostic)
}

func TestResolveHookDiscovery_ConventionalLinkedWorktree(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	initCommittedRepo(t, mainRoot)
	runGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)

	discovery := resolveHookDiscovery(linkedRoot)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, mainRoot), discovery.DiscoveredHooks.Path())
	require.Equal(t, canonicalPathForTest(t, linkedRoot), discovery.worktreeRoot)
	require.True(t, discovery.RepositoryWide)
}

func TestResolveHookDiscovery_OrdinarySubmodule(t *testing.T) {
	t.Parallel()
	ordinaryRoot, _ := setupSubmoduleWorktrees(t)

	discovery := resolveHookDiscovery(ordinaryRoot)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, ordinaryRoot), discovery.DiscoveredHooks.Path())
	require.False(t, discovery.RepositoryWide)
}

func TestResolveHookDiscovery_SeparateGitDirectory(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	storageRoot := filepath.Join(tmp, "git-storage")
	runGitWithDir(t, tmp, "init", "--separate-git-dir", storageRoot, mainRoot)

	discovery := resolveHookDiscovery(mainRoot)
	require.Equal(t, HookDiscoveryResolved, discovery.State)
	require.Equal(t, canonicalHooksPath(t, mainRoot), discovery.DiscoveredHooks.Path())
	require.False(t, discovery.RepositoryWide)
}

func TestResolveHookDiscovery_UnpinnedLayoutsAreUnresolved(t *testing.T) {
	t.Parallel()

	t.Run("bare worktrees", func(t *testing.T) {
		t.Parallel()
		_, _, featureRoot := setupBareWorktreeLayout(t)

		discovery := resolveHookDiscovery(featureRoot)
		require.Equal(t, HookDiscoveryUnresolved, discovery.State)
		require.Empty(t, discovery.DiscoveredHooks.Path())
		require.ErrorContains(t, discovery.Diagnostic, ".bare/worktrees")
	})

	t.Run("linked submodule", func(t *testing.T) {
		t.Parallel()
		_, linkedRoot := setupSubmoduleWorktrees(t)

		discovery := resolveHookDiscovery(linkedRoot)
		require.Equal(t, HookDiscoveryUnresolved, discovery.State)
		require.Empty(t, discovery.DiscoveredHooks.Path())
		require.ErrorContains(t, discovery.Diagnostic, "linked submodules")
	})
}

func TestResolveHookDiscovery_ContradictoryMetadataIsUnresolved(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	movedRoot := filepath.Join(tmp, "moved")
	initCommittedRepo(t, mainRoot)
	runGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)
	require.NoError(t, os.Rename(linkedRoot, movedRoot))

	discovery := resolveHookDiscovery(movedRoot)
	require.Equal(t, HookDiscoveryUnresolved, discovery.State)
	require.Empty(t, discovery.DiscoveredHooks.Path())
	require.ErrorContains(t, discovery.Diagnostic, "worktree registration")
}

func TestResolveHookDiscovery_RefusesUserWideRoot(t *testing.T) {
	fakeHome := t.TempDir()
	initCommittedRepo(t, fakeHome)
	t.Setenv("HOME", fakeHome)
	t.Setenv("CODEX_HOME", "")

	discovery := resolveHookDiscovery(fakeHome)
	require.Equal(t, HookDiscoveryUnresolved, discovery.State)
	var unresolved *UnresolvedHookDiscoveryError
	require.ErrorAs(t, discovery.Diagnostic, &unresolved)
	require.Contains(t, unresolved.Reason, "user-wide")
}

func setupSeparateGitDirWorktree(t *testing.T, tmp, gitDir string) (mainRoot, linkedRoot string) {
	t.Helper()
	mainRoot = filepath.Join(tmp, "main")
	linkedRoot = filepath.Join(tmp, "linked")
	runGitWithDir(t, tmp, "init", "--separate-git-dir", gitDir, mainRoot)
	runGit(t, mainRoot, "config", "user.name", "Entire Test")
	runGit(t, mainRoot, "config", "user.email", "test@entire.io")
	runGit(t, mainRoot, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(mainRoot, "README.md"), []byte("initial\n"), 0o600))
	runGit(t, mainRoot, "add", "README.md")
	runGit(t, mainRoot, "commit", "--no-gpg-sign", "-m", "initial")
	runGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)
	return mainRoot, linkedRoot
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
	require.NoError(t, os.WriteFile(filepath.Join(layoutRoot, ".git"), []byte("gitdir: ./.bare\n"), 0o600))
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
	return filepath.Join(canonicalPathForTest(t, root), ".codex", HooksFileName)
}

func canonicalPathForTest(t *testing.T, path string) string {
	t.Helper()
	canonical, err := canonicalPath(path)
	require.NoError(t, err)
	return canonical
}
