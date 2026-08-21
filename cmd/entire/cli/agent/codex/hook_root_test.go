package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
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

// Without the container's .git pointer file the ownership proof fails, and
// Codex 0.149.0 empirically reads each worktree's local hooks file instead of
// the container's.
func TestResolveHookLocation_PointerlessBareContainerUsesWorktreeLocalHooks(t *testing.T) {
	t.Parallel()
	layoutRoot, mainRoot, featureRoot := setupBareWorktreeLayout(t)
	require.NoError(t, os.Remove(filepath.Join(layoutRoot, ".git")))

	for _, worktreeRoot := range []string{mainRoot, featureRoot} {
		location, err := resolveHookLocation(worktreeRoot)
		require.NoError(t, err)
		require.Equal(t, canonicalHooksPath(t, worktreeRoot), location.HooksPath)
		require.Empty(t, location.LegacyHooksPath)
		require.False(t, location.RepositoryWide)
	}
}

func TestResolveHookLocation_Submodules(t *testing.T) {
	t.Parallel()
	ordinarySubmoduleRoot, linkedSubmoduleRoot := setupSubmoduleWorktrees(t)

	ordinary, err := resolveHookLocation(ordinarySubmoduleRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, ordinarySubmoduleRoot), ordinary.HooksPath)
	require.False(t, ordinary.RepositoryWide)

	// The linked submodule's common directory lives in .git/modules, whose
	// parent owns no .git entry, so Codex reads the worktree-local file.
	linked, err := resolveHookLocation(linkedSubmoduleRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, linkedSubmoduleRoot), linked.HooksPath)
	require.Empty(t, linked.LegacyHooksPath)
	require.False(t, linked.RepositoryWide)
	require.Equal(t, worktreeLocalLockPath(t, linkedSubmoduleRoot), linked.LockPath)
}

// A bare repository's parent directory does not own the common Git directory
// through a .git entry, so Codex reads the worktree-local hooks file there —
// verified against Codex 0.149.0 hooks/list.
func TestResolveHookLocation_BareRepositoryWorktreeUsesWorktreeLocalHooks(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	seedRoot := filepath.Join(tmp, "seed")
	bareRoot := filepath.Join(tmp, "repo.git")
	linkedRoot := filepath.Join(tmp, "linked")
	initCommittedRepo(t, seedRoot)
	runGit(t, tmp, "clone", "--bare", seedRoot, bareRoot)
	runGitWithDir(t, tmp, "--git-dir", bareRoot, "worktree", "add", "-b", "feature", linkedRoot)

	location, err := resolveHookLocation(linkedRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, linkedRoot), location.HooksPath)
	require.Empty(t, location.LegacyHooksPath)
	require.Equal(t, worktreeLocalLockPath(t, linkedRoot), location.LockPath)
	require.False(t, location.RepositoryWide)
}

func TestResolveHookLocation_RefusesUserHomeAsHookRoot(t *testing.T) {
	fakeHome := t.TempDir()
	initCommittedRepo(t, fakeHome)
	t.Setenv("HOME", fakeHome)
	t.Setenv("CODEX_HOME", "")

	_, err := resolveHookLocation(fakeHome)
	var unsupported *UnsupportedHookLocationError
	require.ErrorAs(t, err, &unsupported)
	require.Equal(t, canonicalPathForTest(t, fakeHome), unsupported.HookRoot)
	require.Empty(t, unsupported.Location.LegacyHooksPath)
}

// A detached --separate-git-dir storage directory leaves the common
// directory's parent without a .git entry, so the ownership proof fails and
// Codex reads the worktree-local hooks file.
func TestResolveHookLocation_SeparateGitDirWorktreeUsesWorktreeLocalHooks(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainRoot, linkedRoot := setupSeparateGitDirWorktree(t, tmp, filepath.Join(tmp, "git-storage"))

	location, err := resolveHookLocation(linkedRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, linkedRoot), location.HooksPath)
	require.Empty(t, location.LegacyHooksPath)
	require.False(t, location.RepositoryWide)
	require.NotEqual(t, canonicalHooksPath(t, mainRoot), location.HooksPath)
}

// When the separate Git directory is named .git, its parent passes Codex's
// ownership proof, and Codex 0.149.0 empirically loads hooks.json from that
// storage parent for linked-worktree sessions — never from the main checkout,
// whose location Git records nowhere reachable from the worktree. Entire must
// install where Codex reads.
func TestResolveHookLocation_SeparateGitDirNamedDotGitPromotesStorageParent(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	storageRoot := filepath.Join(tmp, "storage")
	require.NoError(t, os.MkdirAll(storageRoot, 0o750))
	mainRoot, linkedRoot := setupSeparateGitDirWorktree(t, tmp, filepath.Join(storageRoot, ".git"))

	location, err := resolveHookLocation(linkedRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, storageRoot), location.HooksPath)
	require.Equal(t, canonicalHooksPath(t, linkedRoot), location.LegacyHooksPath)
	require.Equal(t, canonicalLockPath(t, storageRoot), location.LockPath)
	require.True(t, location.RepositoryWide)
	require.NotEqual(t, canonicalHooksPath(t, mainRoot), location.HooksPath)
}

// A moved worktree whose registration backlink was not repaired fails Codex's
// round-trip proof, so hooks stay worktree-local until `git worktree repair`.
func TestResolveHookLocation_MovedWorktreeFallsBackToWorktreeLocalHooks(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "repo")
	linkedRoot := filepath.Join(tmp, "linked")
	movedRoot := filepath.Join(tmp, "moved")
	initCommittedRepo(t, repoRoot)
	runGit(t, repoRoot, "worktree", "add", "-b", "feature", linkedRoot)
	require.NoError(t, os.Rename(linkedRoot, movedRoot))

	location, err := resolveHookLocation(movedRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, movedRoot), location.HooksPath)
	require.Empty(t, location.LegacyHooksPath)
	require.False(t, location.RepositoryWide)

	runGit(t, repoRoot, "worktree", "repair", movedRoot)
	repaired, err := resolveHookLocation(movedRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalHooksPath(t, repoRoot), repaired.HooksPath)
	require.True(t, repaired.RepositoryWide)
}

func TestResolveHookLocation_InvalidLinkedWorktreeMetadataFallsBackToLocalHooks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, linkedRoot, gitDir string)
	}{
		{
			name: "symlinked dot git file",
			mutate: func(t *testing.T, linkedRoot, _ string) {
				dotGit := filepath.Join(linkedRoot, ".git")
				target := filepath.Join(linkedRoot, "gitfile-target")
				require.NoError(t, os.Rename(dotGit, target))
				require.NoError(t, os.Symlink(filepath.Base(target), dotGit))
			},
		},
		{
			name: "oversized dot git file",
			mutate: func(t *testing.T, linkedRoot, _ string) {
				dotGit := filepath.Join(linkedRoot, ".git")
				contents := readFile(t, dotGit) + strings.Repeat(" ", 64*1024)
				require.NoError(t, os.WriteFile(dotGit, []byte(contents), 0o600))
			},
		},
		{
			name: "symlinked registration backlink",
			mutate: func(t *testing.T, _, gitDir string) {
				backlink := filepath.Join(gitDir, "gitdir")
				target := filepath.Join(gitDir, "gitdir-target")
				require.NoError(t, os.Rename(backlink, target))
				require.NoError(t, os.Symlink(filepath.Base(target), backlink))
			},
		},
		{
			name: "symlinked common directory file",
			mutate: func(t *testing.T, _, gitDir string) {
				commondir := filepath.Join(gitDir, "commondir")
				target := filepath.Join(gitDir, "commondir-target")
				require.NoError(t, os.Rename(commondir, target))
				require.NoError(t, os.Symlink(filepath.Base(target), commondir))
			},
		},
		{
			name: "missing common directory file",
			mutate: func(t *testing.T, _, gitDir string) {
				require.NoError(t, os.Remove(filepath.Join(gitDir, "commondir")))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			repoRoot := filepath.Join(tmp, "repo")
			linkedRoot := filepath.Join(tmp, "linked")
			initCommittedRepo(t, repoRoot)
			runGit(t, repoRoot, "worktree", "add", "-b", "feature", linkedRoot)
			gitDir, err := readGitDirFile(filepath.Join(linkedRoot, ".git"), linkedRoot)
			require.NoError(t, err)
			test.mutate(t, linkedRoot, gitDir)

			location, err := resolveHookLocation(linkedRoot)
			require.NoError(t, err)
			require.Equal(t, canonicalHooksPath(t, linkedRoot), location.HooksPath)
			require.Empty(t, location.LegacyHooksPath)
			require.False(t, location.RepositoryWide)
			require.Equal(t, worktreeLocalLockPath(t, linkedRoot), location.LockPath)
		})
	}
}

// The promoted hook root is refused when it would collide with CODEX_HOME,
// where a hooks.json merges into every Codex session machine-wide.
func TestResolveHookLocation_LinkedWorktreeRefusesCodexHomeCollision(t *testing.T) {
	tmp := t.TempDir()
	repoRoot := filepath.Join(tmp, "repo")
	linkedRoot := filepath.Join(tmp, "linked")
	initCommittedRepo(t, repoRoot)
	runGit(t, repoRoot, "worktree", "add", "-b", "feature", linkedRoot)
	require.NoError(t, os.MkdirAll(filepath.Join(repoRoot, ".codex"), 0o750))
	t.Setenv("CODEX_HOME", filepath.Join(repoRoot, ".codex"))

	_, err := resolveHookLocation(linkedRoot)
	var unsupported *UnsupportedHookLocationError
	require.ErrorAs(t, err, &unsupported)
	require.Equal(t, canonicalPathForTest(t, repoRoot), unsupported.HookRoot)
	require.Equal(t, canonicalHooksPath(t, linkedRoot), unsupported.Location.LegacyHooksPath)
	require.Equal(t, canonicalLockPath(t, repoRoot), unsupported.Location.LockPath)
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
	// The container's .git pointer file is what lets the container pass
	// Codex's ownership proof; the pointer-less variant removes it.
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

func canonicalLockPath(t *testing.T, root string) string {
	t.Helper()
	return filepath.Join(canonicalPathForTest(t, root), ".git", "entire-codex-hooks.lock")
}

// worktreeLocalLockPath resolves the lock beside a linked worktree's local
// hooks file when Codex's shared-root ownership proof fails.
func worktreeLocalLockPath(t *testing.T, worktreeRoot string) string {
	t.Helper()
	return canonicalHooksPath(t, worktreeRoot) + ".lock"
}

func canonicalPathForTest(t *testing.T, path string) string {
	t.Helper()
	canonical, err := canonicalPath(path)
	require.NoError(t, err)
	return canonical
}
