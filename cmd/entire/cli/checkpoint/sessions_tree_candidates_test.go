package checkpoint

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetSessionsBranchTree_ReadCandidateFallback pins the committed-read
// tracking-ref fallback: with no local Read ref, the store consults the
// injected read-candidate remotes' tracking refs in order (first existing
// wins) — a pure read. With no chain injected, the legacy origin-only
// fallback holds.
// Not parallel: IsolateGitConfigEnv uses t.Setenv.
func TestGetSessionsBranchTree_ReadCandidateFallback(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")

	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	headHash := strings.TrimSpace(string(out))
	testutil.GitUpdateRef(t, dir, "refs/remotes/upstream/"+DefaultV1Refs().Primary.Short(), headHash)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	// No chain injected: the legacy origin-only fallback misses the
	// upstream tracking ref.
	legacyStore := NewGitStore(repo, DefaultV1Refs())
	_, err = legacyStore.getSessionsBranchTree()
	require.Error(t, err, "legacy origin-only fallback must not see upstream's tracking ref")

	// Candidate chain injected: upstream's tracking ref serves the read.
	store := NewGitStore(repo, DefaultV1Refs())
	store.SetReadRemotes([]string{"upstream", "origin"})
	tree, err := store.getSessionsBranchTree()
	require.NoError(t, err)
	_, err = tree.File("f.txt")
	assert.NoError(t, err)

	// A local Read ref still wins over any tracking ref.
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(DefaultV1Refs().Read, plumbing.NewHash(headHash))))
	tree, err = store.getSessionsBranchTree()
	require.NoError(t, err)
	require.NotNil(t, tree)
}
