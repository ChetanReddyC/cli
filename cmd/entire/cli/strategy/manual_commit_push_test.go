package strategy

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/require"
)

func TestPushTargetsFingerprint_OrderIndependent(t *testing.T) {
	t.Parallel()

	a := pushTargetsFingerprint([]string{"https://example.com/a.git", "https://example.com/b.git"})
	b := pushTargetsFingerprint([]string{"https://example.com/b.git", "https://example.com/a.git"})
	require.Equal(t, a, b, "fingerprint must not depend on target order")

	c := pushTargetsFingerprint([]string{"https://example.com/a.git"})
	require.NotEqual(t, a, c, "a different target set must produce a different fingerprint")
}

// TestDeferCheckpointPushOnEmptyRemote_BootstrapMarkerSkipsNetwork proves the
// memoized fast path avoids the ls-remote probe. The remote points at an
// unreachable path, so the guard can only avoid deferring by short-circuiting
// on the persisted bootstrap marker rather than inspecting the remote.
func TestDeferCheckpointPushOnEmptyRemote_BootstrapMarkerSkipsNetwork(t *testing.T) {
	// No t.Parallel: uses t.Chdir and the process-wide worktree-root cache.
	dir := t.TempDir()
	testutil.InitRepo(t, dir)

	// A local path that does not exist: ls-remote fails fast, no network.
	unreachable := t.TempDir() + "/nonexistent-remote.git"
	add := exec.CommandContext(t.Context(), "git", "remote", "add", "origin", unreachable)
	add.Dir = dir
	require.NoError(t, add.Run())

	t.Chdir(dir)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	ctx := context.Background()
	ps := pushSettings{remote: "origin"}

	// Without a marker the guard must probe the (unreachable) remote and fail
	// closed to deferral.
	require.True(t, deferCheckpointPushOnEmptyRemote(ctx, ps),
		"an unreachable remote with no marker must fail closed to deferral")

	// Record the marker for the resolved push targets, then the guard must
	// short-circuit to "publish" without touching the unreachable remote.
	targets, err := checkpointremote.PushTargetsInDir(ctx, dir, "origin")
	require.NoError(t, err)
	writePushBootstrapMarker(ctx, pushTargetsFingerprint(targets))

	require.False(t, deferCheckpointPushOnEmptyRemote(ctx, ps),
		"the bootstrap marker must short-circuit the network probe")

	// A marker whose fingerprint doesn't match the current targets must not
	// short-circuit: it falls back to probing and defers on the unreachable remote.
	writePushBootstrapMarker(ctx, "stale-fingerprint")
	require.True(t, deferCheckpointPushOnEmptyRemote(ctx, ps),
		"a marker that does not match the current targets must not short-circuit")

	// An expired marker must not be trusted even when the fingerprint matches:
	// the remote could have been emptied/recreated since, so it is re-probed
	// (and defers on the unreachable remote). Backdate the file past the TTL.
	writePushBootstrapMarker(ctx, pushTargetsFingerprint(targets))
	markerPath, err := pushBootstrapMarkerPath(ctx)
	require.NoError(t, err)
	stale := time.Now().Add(-pushBootstrapTTL - time.Minute)
	require.NoError(t, os.Chtimes(markerPath, stale, stale))
	require.True(t, deferCheckpointPushOnEmptyRemote(ctx, ps),
		"an expired marker must be re-validated, not trusted indefinitely")
}
