package checkpoint

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

	store := NewGitStore(repo, DefaultV1Refs())
	store.SetReadRemotes([]string{"upstream", "origin"})
	tree, err := store.getSessionsBranchTree()
	require.NoError(t, err)
	_, err = tree.File("f.txt")
	assert.NoError(t, err)
}
