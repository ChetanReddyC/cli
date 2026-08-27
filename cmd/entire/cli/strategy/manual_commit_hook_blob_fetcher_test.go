package strategy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// TestHookCheckpointStoreOptions_CarriesBothBounds is the guard for the shape
// that broke: a hook store that fetches missing refs but not missing blobs
// resolves a checkpoint's ref and then reports the checkpoint itself missing on
// any partial clone.
func TestHookCheckpointStoreOptions_CarriesBothBounds(t *testing.T) {
	t.Parallel()

	s := NewManualCommitStrategy()
	s.SetBlobFetcher(func(context.Context, []plumbing.Hash) error { return nil })

	opts := s.hookCheckpointStoreOptions(context.Background())

	require.NotNil(t, opts.RefFetcher, "hook reads must be able to fetch a ref that exists only on the remote")
	require.NotNil(t, opts.BlobFetcher, "hook reads must be able to fetch a blob a partial clone filtered out")
}

func TestHookBlobFetcher_NilWithoutBlobFetcher(t *testing.T) {
	t.Parallel()

	s := NewManualCommitStrategy()

	require.Nil(t, s.hookBlobFetcher(), "no fetcher to bound means no fetcher to hand the store")
}

// TestHookBlobFetcher_BoundsTheCallAndPassesHashes pins the envelope the hook
// paths rely on: the interactive read chain's minutes must not be inherited by
// a hook the user's git command is waiting on, and SSH must not be able to
// prompt for a passphrase there.
func TestHookBlobFetcher_BoundsTheCallAndPassesHashes(t *testing.T) {
	t.Parallel()

	want := []plumbing.Hash{
		plumbing.NewHash("1111111111111111111111111111111111111111"),
		plumbing.NewHash("2222222222222222222222222222222222222222"),
	}
	sentinel := errors.New("fetch failed")

	var (
		gotHashes   []plumbing.Hash
		gotDeadline time.Time
		gotBatchSSH bool
		called      int
	)
	s := NewManualCommitStrategy()
	s.SetBlobFetcher(func(ctx context.Context, hashes []plumbing.Hash) error {
		called++
		gotHashes = hashes
		gotDeadline, _ = ctx.Deadline()
		gotBatchSSH = remote.IsNonInteractiveSSH(ctx)
		return sentinel
	})

	fetch := s.hookBlobFetcher()
	require.NotNil(t, fetch)
	require.ErrorIs(t, fetch(context.Background(), want), sentinel, "the store must still see why a fetch failed")

	require.Equal(t, 1, called)
	require.Equal(t, want, gotHashes)
	require.True(t, gotBatchSSH, "a hook fetch must not be able to prompt for an SSH passphrase")
	require.WithinDuration(t, time.Now().Add(remote.WriteProbeFetchBudget), gotDeadline, remote.WriteProbeFetchBudget,
		"the hook fetch must carry the write-probe budget, not the interactive read chain's")
	require.Less(t, time.Until(gotDeadline), remote.ReadChainBudget,
		"the hook fetch must be bounded well inside the interactive read chain")
}

// TestHookBlobFetcher_DoesNotExtendACallerDeadline guards the direction that
// matters when the hook itself is already running out of time (an agent's
// session-end budget): bounding must never push a deadline out.
func TestHookBlobFetcher_DoesNotExtendACallerDeadline(t *testing.T) {
	t.Parallel()

	var gotDeadline time.Time
	s := NewManualCommitStrategy()
	s.SetBlobFetcher(func(ctx context.Context, _ []plumbing.Hash) error {
		gotDeadline, _ = ctx.Deadline()
		return nil
	})

	tight := 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), tight)
	defer cancel()
	require.NoError(t, s.hookBlobFetcher()(ctx, []plumbing.Hash{plumbing.ZeroHash}))

	require.LessOrEqual(t, time.Until(gotDeadline), tight)
}
