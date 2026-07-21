package remote

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
)

// WriteProbeFetchBudget bounds the on-demand ref fetch performed by a
// BACKFILL's absence probe. Write paths run inside git hooks (post-commit,
// stop-time finalize) where a dead network must not stall the user's
// workflow for the read path's full fetch window; combined with the
// per-store failure memo in the git-refs store, a loop over N checkpoints
// pays a dead network once, briefly.
const WriteProbeFetchBudget = 15 * time.Second

// readFetchTimeout bounds the interactive read-path fetch (unchanged from
// the historical FetchCheckpointRef behavior).
const readFetchTimeout = 2 * time.Minute

// CheckpointFetchTarget returns the git remote (URL or name) that checkpoint
// data is fetched from. It prefers the effective URL resolved by FetchURL,
// which is the source of truth for checkpoint fetch location. If URL
// resolution fails, it falls back to the origin remote name so callers can
// still attempt a fetch.
func CheckpointFetchTarget(ctx context.Context) string {
	url, err := FetchURL(ctx)
	if err == nil && url != "" {
		return url
	}
	return "origin"
}

// FetchCheckpointRef fetches a single per-checkpoint ref
// (refs/entire/checkpoints/<shard>/<id>) from the checkpoint remote into the
// local ref of the same name, so the git-refs store can resolve a checkpoint
// written on another machine.
//
// Contract — absence is distinguishable from failure:
//   - The remote genuinely lacking the ref returns an error wrapping
//     plumbing.ErrReferenceNotFound (probed via ls-remote before fetching,
//     because `git fetch` of a missing refspec fails indistinguishably from a
//     transport error). Store probes classify this as "checkpoint not found",
//     which write routing may legitimately act on.
//   - Any transport-level failure (probe or fetch) is surfaced as a real
//     error, never mapped to absence — a false "absent" would misdirect a
//     backfill onto another backend instead of retrying.
func FetchCheckpointRef(ctx context.Context, ref plumbing.ReferenceName) error {
	ctx, cancel := context.WithTimeout(ctx, readFetchTimeout)
	defer cancel()

	fetchTarget := CheckpointFetchTarget(ctx)

	out, err := LsRemoteInDir(ctx, "", fetchTarget, ref.String())
	if err != nil {
		// Redact: fetchTarget can be a remote URL with embedded credentials
		// (CI origin URLs), and this error is logged and shown to users.
		return fmt.Errorf("probe checkpoint ref %s on %s: %w", ref, RedactURL(fetchTarget), err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return fmt.Errorf("checkpoint ref %s not found on %s: %w", ref, RedactURL(fetchTarget), plumbing.ErrReferenceNotFound)
	}

	refSpec := "+" + ref.String() + ":" + ref.String()
	if _, err := Fetch(ctx, FetchOptions{
		Remote:   fetchTarget,
		RefSpecs: []string{refSpec},
		NoTags:   true,
	}); err != nil {
		return fmt.Errorf("fetch checkpoint ref %s from %s: %w", ref, RedactURL(fetchTarget), err)
	}
	return nil
}

// BoundedCheckpointRefFetcher returns a RefFetchFunc-shaped fetcher whose
// per-call budget is capped at d, for wiring into write-path checkpoint
// stores (see WriteProbeFetchBudget).
func BoundedCheckpointRefFetcher(d time.Duration) func(context.Context, plumbing.ReferenceName) error {
	return func(ctx context.Context, ref plumbing.ReferenceName) error {
		ctx, cancel := context.WithTimeout(ctx, d)
		defer cancel()
		return FetchCheckpointRef(ctx, ref)
	}
}
