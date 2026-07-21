package remote

import (
	"context"
	"os/exec"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// checkpointRefFixture creates a work repo whose origin is a local bare repo
// holding (or not holding) a checkpoint ref, and chdirs into the work repo so
// fetch-target resolution finds origin.
func checkpointRefFixture(t *testing.T, withRef bool) (workDir string, ref plumbing.ReferenceName) {
	t.Helper()
	bareDir := t.TempDir()
	out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", bareDir).CombinedOutput()
	require.NoError(t, err, "git init --bare: %s", out)

	workDir = t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	out, err = exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "add", "origin", bareDir).CombinedOutput()
	require.NoError(t, err, "git remote add: %s", out)

	ref = plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	if withRef {
		out, err = exec.CommandContext(t.Context(), "git", "-C", workDir, "push", "--quiet", "origin", "HEAD:"+ref.String()).CombinedOutput()
		require.NoError(t, err, "git push checkpoint ref: %s", out)
	}

	t.Chdir(workDir)
	return workDir, ref
}

// TestFetchCheckpointRef_RemoteMissingRefIsAbsence: a remote that does not
// have the requested checkpoint ref is ABSENCE, not a transport failure — the
// error must wrap plumbing.ErrReferenceNotFound so store probes (reads and
// backfill writes) classify it as "checkpoint not found" and, under kind
// routing, may legitimately fall through to another backend. Before this
// distinction, git fetch of a missing refspec failed like a network error,
// which made wiring a fetcher into write paths unsafe.
func TestFetchCheckpointRef_RemoteMissingRefIsAbsence(t *testing.T) {
	_, ref := checkpointRefFixture(t, false)

	err := FetchCheckpointRef(context.Background(), ref)
	require.Error(t, err)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a ref the remote does not have must classify as absence")
}

// TestFetchCheckpointRef_PresentRefFetches: the ref exists on the remote but
// not locally; the fetch must create the local ref of the same name.
func TestFetchCheckpointRef_PresentRefFetches(t *testing.T) {
	workDir, ref := checkpointRefFixture(t, true)

	require.NoError(t, FetchCheckpointRef(context.Background(), ref))

	out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "show-ref", "--verify", ref.String()).CombinedOutput()
	require.NoError(t, err, "fetched ref must exist locally: %s", out)
}

// TestFetchCheckpointRef_UnreachableRemoteIsFailure: a transport-level
// failure (unreachable remote) must NOT classify as absence.
func TestFetchCheckpointRef_UnreachableRemoteIsFailure(t *testing.T) {
	workDir, ref := checkpointRefFixture(t, false)
	out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "set-url", "origin", workDir+"/nonexistent-remote").CombinedOutput()
	require.NoError(t, err, "%s", out)

	err = FetchCheckpointRef(context.Background(), ref)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a transport failure must stay distinguishable from absence")
}
