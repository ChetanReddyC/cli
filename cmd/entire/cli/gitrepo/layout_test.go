package gitrepo_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

func TestResolveGitLayout_ClassifiesSupportedStructures(t *testing.T) {
	t.Parallel()

	t.Run("normal checkout", func(t *testing.T) {
		t.Parallel()
		root := filepath.Join(t.TempDir(), "repo")
		initLayoutRepo(t, root)

		layout, err := gitrepo.ResolveGitLayoutAt(root)
		require.NoError(t, err)
		require.Equal(t, gitrepo.GitLayoutNormal, layout.Kind)
		require.Equal(t, canonicalLayoutPath(t, root), layout.WorktreeRoot)
		require.Equal(t, canonicalLayoutPath(t, filepath.Join(root, ".git")), layout.CommonDir)
		require.Equal(t, layout.WorktreeRoot, layout.MainWorktreeRoot)
	})

	t.Run("conventional linked worktree", func(t *testing.T) {
		t.Parallel()
		tmp := t.TempDir()
		mainRoot := filepath.Join(tmp, "main")
		linkedRoot := filepath.Join(tmp, "linked")
		initLayoutRepo(t, mainRoot)
		runLayoutGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)

		layout, err := gitrepo.ResolveGitLayoutAt(linkedRoot)
		require.NoError(t, err)
		require.Equal(t, gitrepo.GitLayoutLinkedWorktree, layout.Kind)
		require.Equal(t, canonicalLayoutPath(t, mainRoot), layout.MainWorktreeRoot)
		require.Equal(t, canonicalLayoutPath(t, filepath.Join(mainRoot, ".git")), layout.CommonDir)
		require.NotEmpty(t, layout.WorktreeID)
	})

	t.Run("bare worktrees container", func(t *testing.T) {
		t.Parallel()
		tmp := t.TempDir()
		seedRoot := filepath.Join(tmp, "seed")
		containerRoot := filepath.Join(tmp, "container")
		bareRoot := filepath.Join(containerRoot, ".bare")
		worktreeRoot := filepath.Join(containerRoot, "feature")
		initLayoutRepo(t, seedRoot)
		require.NoError(t, os.MkdirAll(containerRoot, 0o750))
		runLayoutGit(t, tmp, "clone", "--bare", seedRoot, bareRoot)
		require.NoError(t, os.WriteFile(filepath.Join(containerRoot, ".git"), []byte("gitdir: ./.bare\n"), 0o600))
		runLayoutGitArgs(t, tmp, "--git-dir", bareRoot, "worktree", "add", "-b", "feature", worktreeRoot)

		layout, err := gitrepo.ResolveGitLayoutAt(worktreeRoot)
		require.NoError(t, err)
		require.Equal(t, gitrepo.GitLayoutBareWorktree, layout.Kind)
		require.Equal(t, canonicalLayoutPath(t, containerRoot), layout.MainWorktreeRoot)
	})

	t.Run("ordinary and linked submodule", func(t *testing.T) {
		t.Parallel()
		tmp := t.TempDir()
		subjectRoot := filepath.Join(tmp, "subject")
		superRoot := filepath.Join(tmp, "super")
		ordinaryRoot := filepath.Join(superRoot, "sub")
		linkedRoot := filepath.Join(tmp, "linked-sub")
		initLayoutRepo(t, subjectRoot)
		initLayoutRepo(t, superRoot)
		runLayoutGit(t, superRoot, "-c", "protocol.file.allow=always", "submodule", "add", subjectRoot, "sub")
		testutil.GitAdd(t, superRoot, ".gitmodules", "sub")
		testutil.GitCommit(t, superRoot, "add submodule")
		runLayoutGit(t, ordinaryRoot, "worktree", "add", "-b", "linked", linkedRoot)

		ordinary, err := gitrepo.ResolveGitLayoutAt(ordinaryRoot)
		require.NoError(t, err)
		require.Equal(t, gitrepo.GitLayoutSubmodule, ordinary.Kind)

		linked, err := gitrepo.ResolveGitLayoutAt(linkedRoot)
		require.NoError(t, err)
		require.Equal(t, gitrepo.GitLayoutLinkedSubmodule, linked.Kind)
		require.NotEmpty(t, linked.WorktreeID)
	})

	t.Run("separate Git directory", func(t *testing.T) {
		t.Parallel()
		tmp := t.TempDir()
		root := filepath.Join(tmp, "repo")
		storage := filepath.Join(tmp, "storage")
		runLayoutGitArgs(t, tmp, "init", "--separate-git-dir", storage, root)

		layout, err := gitrepo.ResolveGitLayoutAt(root)
		require.NoError(t, err)
		require.Equal(t, gitrepo.GitLayoutSeparateGitDir, layout.Kind)
		require.Equal(t, canonicalLayoutPath(t, storage), layout.CommonDir)
		require.Empty(t, layout.MainWorktreeRoot)
	})
}

func TestResolveGitLayout_RejectsContradictoryLinkedMetadata(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	mainRoot := filepath.Join(tmp, "main")
	linkedRoot := filepath.Join(tmp, "linked")
	movedRoot := filepath.Join(tmp, "moved")
	initLayoutRepo(t, mainRoot)
	runLayoutGit(t, mainRoot, "worktree", "add", "-b", "feature", linkedRoot)
	require.NoError(t, os.Rename(linkedRoot, movedRoot))

	layout, err := gitrepo.ResolveGitLayoutAt(movedRoot)
	require.Error(t, err)
	require.Equal(t, gitrepo.GitLayoutUnresolved, layout.Kind)
}

func TestResolveGitLayout_MemoizesByWorktreeRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "repo")
	initLayoutRepo(t, root)

	first, err := gitrepo.ResolveGitLayoutAt(root)
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(root, ".git"), filepath.Join(root, ".git-away")))

	second, err := gitrepo.ResolveGitLayoutAt(root)
	require.NoError(t, err)
	require.Equal(t, first, second)
}

func initLayoutRepo(t *testing.T, root string) {
	t.Helper()
	testutil.InitRepo(t, root)
	testutil.WriteFile(t, root, "README.md", "initial\n")
	testutil.GitAdd(t, root, "README.md")
	testutil.GitCommit(t, root, "initial")
}

func runLayoutGit(t *testing.T, root string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", root}, args...)
	runLayoutGitArgs(t, root, commandArgs...)
}

func runLayoutGitArgs(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = testutil.GitIsolatedEnv()
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func canonicalLayoutPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	abs, err := filepath.Abs(resolved)
	require.NoError(t, err)
	return filepath.Clean(abs)
}
