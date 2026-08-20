package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

func TestConcurrentHookMutationsPreserveUserHooks(t *testing.T) {
	tempDir := setupTestEnv(t)
	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	lockPath := hooksPath + ".lock"
	require.NoError(t, os.MkdirAll(filepath.Dir(hooksPath), 0o750))
	require.NoError(t, os.WriteFile(hooksPath, []byte(`{"hooks":{}}`), 0o600))

	const mutations = 24
	ag := &CodexAgent{}
	start := make(chan struct{})
	errs := make(chan error, mutations*2)
	var wg sync.WaitGroup
	for i := range mutations {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			<-start
			var err error
			if i%2 == 0 {
				_, err = ag.InstallHooks(t.Context(), false)
			} else {
				err = ag.UninstallHooks(t.Context())
			}
			if err != nil {
				errs <- err
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := addUserHookWithLock(t.Context(), lockPath, hooksPath, fmt.Sprintf("user-hook-%d", i)); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	require.True(t, json.Valid(data))
	for i := range mutations {
		require.Contains(t, string(data), fmt.Sprintf("user-hook-%d", i))
	}
}

func addUserHookWithLock(ctx context.Context, lockPath, hooksPath, command string) error {
	release, err := acquireHooksLock(ctx, lockPath)
	if err != nil {
		return err
	}
	defer release()

	document, err := readHooksDocument(hooksPath)
	if err != nil {
		return err
	}
	var groups []MatcherGroup
	if err := parseHookType(document.rawHooks, "Stop", &groups); err != nil {
		return err
	}
	marshalHookType(document.rawHooks, "Stop", addHook(groups, command, defaultHookTimeoutSec))
	return writeHooksDocument(hooksPath, document)
}
