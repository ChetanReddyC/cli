package codex

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/require"
)

func TestAcquireHooksLockWithinTimesOut(t *testing.T) {
	t.Parallel()

	lockPath := filepath.Join(t.TempDir(), "hooks.lock")
	holder := flock.New(lockPath)
	locked, err := holder.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	t.Cleanup(func() { require.NoError(t, holder.Unlock()) })

	start := time.Now()
	release, err := acquireHooksLockWithin(t.Context(), lockPath, 50*time.Millisecond)
	require.Nil(t, release)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "timed out after 50ms waiting for another Entire process to update Codex hooks")
	require.Less(t, time.Since(start), 2*time.Second)
}

func TestAcquireHooksLockWithinPreservesCallerCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	release, err := acquireHooksLockWithin(ctx, filepath.Join(t.TempDir(), "hooks.lock"), time.Minute)
	require.Nil(t, release)
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, err.Error(), "timed out")
}
