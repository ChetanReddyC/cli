package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// initReftableRepo creates a repository using the reftable ref backend via the
// git CLI, or skips the test when the installed git is too old to support it.
// It returns the repo dir and the initial commit hash.
func initReftableRepo(t *testing.T, name, content string) (string, string) {
	t.Helper()
	repoDir := t.TempDir()

	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)

	initCmd := exec.Command("git", "init", "-b", "main", "--ref-format=reftable", repoDir) //nolint:noctx // test capability probe
	initCmd.Env = env
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Skipf("git does not support reftable repositories: %v\n%s", err, out)
	}

	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...) //nolint:noctx // test helper
		cmd.Dir = repoDir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
		return strings.TrimSpace(string(out))
	}

	if got := git("rev-parse", "--show-ref-format"); got != "reftable" {
		t.Skipf("git initialized ref format %q, not reftable", got)
	}
	git("config", "user.name", "Test User")
	git("config", "user.email", "test@example.com")
	git("config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, name), []byte(content), 0o644))
	git("add", name)
	git("commit", "-m", "initial")

	return repoDir, git("rev-parse", "HEAD")
}

// TestOpenPath_ReftableRepository verifies that a reftable repository, which
// go-git's filesystem storer cannot open, is opened successfully and that ref
// read/write/list/remove all round-trip through the git-CLI-backed storer.
func TestOpenPath_ReftableRepository(t *testing.T) {
	t.Parallel()
	repoDir, headHash := initReftableRepo(t, "file.txt", "hello\n")

	repo, err := OpenPath(repoDir)
	require.NoError(t, err, "reftable repository should open")
	defer repo.Close()

	// HEAD resolves to the real branch, not the reftable .invalid stub.
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, "refs/heads/main", head.Name().String())
	require.Equal(t, headHash, head.Hash().String())

	// Write a new ref via go-git (routed through git update-ref) and read it back.
	newRef := plumbing.NewHashReference(plumbing.ReferenceName("refs/entire/test/one"), head.Hash())
	require.NoError(t, repo.Storer.SetReference(newRef))

	got, err := repo.Storer.Reference(newRef.Name())
	require.NoError(t, err)
	require.Equal(t, head.Hash(), got.Hash())

	// The new ref appears in iteration.
	iter, err := repo.Storer.IterReferences()
	require.NoError(t, err)
	found := false
	require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
		if r.Name() == newRef.Name() {
			found = true
		}
		return nil
	}))
	iter.Close()
	require.True(t, found, "written ref should appear in IterReferences")

	// Removal round-trips, and removing again is a no-op.
	require.NoError(t, repo.Storer.RemoveReference(newRef.Name()))
	_, err = repo.Storer.Reference(newRef.Name())
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
	require.NoError(t, repo.Storer.RemoveReference(newRef.Name()))
}

// TestRepoUsesReftable_Detection checks that reftable detection distinguishes
// reftable repositories from classic files-backend repositories.
func TestRepoUsesReftable_Detection(t *testing.T) {
	t.Parallel()

	reftableRepo, _ := initReftableRepo(t, "a.txt", "a\n")
	require.True(t, repoUsesReftable(filepath.Join(reftableRepo, ".git"), filepath.Join(reftableRepo, ".git")))

	filesRepo := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(filesRepo, ".git", "refs"), 0o755))
	require.False(t, repoUsesReftable(filepath.Join(filesRepo, ".git"), filepath.Join(filesRepo, ".git")))
}
