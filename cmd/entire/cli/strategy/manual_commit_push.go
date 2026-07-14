package strategy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	git "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	checkpointremote "github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/perf"
	"github.com/entireio/cli/redact"
)

// errOPFAbortedByUser is returned when the user chose Abort (or pressed
// Ctrl-C) at the OPF prompt. PrePush returns it verbatim; the hook
// command propagates the non-zero exit code so git push aborts.
var errOPFAbortedByUser = errors.New("OPF prompt aborted by user; push cancelled")

var opfPrePushProgressWriter io.Writer = os.Stderr

// PrePush is called by the git pre-push hook before pushing to a remote.
// It pushes each ref in refs.Push alongside the user's push.
//
// If a checkpoint_remote is configured in settings, checkpoint branches/refs
// are pushed to the derived URL instead of the user's push remote.
//
// Configuration options (stored in .entire/settings.json under strategy_options):
//   - push_sessions: false to disable automatic pushing of checkpoints
//   - checkpoint_remote: {"provider": "github", "repo": "org/repo"} to push to a separate repo
func (s *ManualCommitStrategy) PrePush(ctx context.Context, remote string) error {
	return s.prePush(ctx, remote, false)
}

// PrePushFromGitHook handles a push initiated by Git's pre-push hook. Unlike
// direct callers, it protects an empty user remote from receiving checkpoint
// metadata before the user's first normal branch is published.
func (s *ManualCommitStrategy) PrePushFromGitHook(ctx context.Context, remote string) error {
	return s.prePush(ctx, remote, true)
}

func (s *ManualCommitStrategy) prePush(ctx context.Context, remote string, protectFirstUserBranch bool) error {
	// Load settings once for remote resolution and push_sessions check.
	// Spanned because checkpoint-remote resolution can perform a one-time
	// network fetch of the metadata branch (fetchMetadataBranchIfMissing),
	// which is otherwise invisible in the pre-push trace.
	resolveCtx, resolveSpan := perf.Start(ctx, "resolve_push_settings")
	ps := resolvePushSettings(resolveCtx, remote)
	resolveSpan.End()

	if ps.pushDisabled {
		return nil
	}
	deferAutomaticCheckpointPush := protectFirstUserBranch && deferCheckpointPushOnEmptyRemote(ctx, ps)

	// git-refs primary: push the per-checkpoint refs recorded in the push queue
	// instead of the single v1 branch. (A configured git-branch mirror's v1 ref
	// is not pushed here yet — mirror push for downgrade safety is a later step.)
	if cpCfg, _ := settings.LoadCheckpointsConfig(ctx); checkpoint.PrimaryIsRefs(cpCfg) { //nolint:errcheck // fail-soft: a bad checkpoints block already surfaces via Open; default to no refs push
		if deferAutomaticCheckpointPush {
			return nil
		}
		return s.prePushCheckpointRefs(ctx, ps)
	}

	refs := checkpoint.ResolveRefs(ctx)
	repo, repoErr := OpenRepository(ctx)
	if repoErr != nil {
		logging.Warn(ctx, "checkpoint policy pre-push: failed to open repository; allowing checkpoint push",
			slog.String("error", repoErr.Error()),
		)
	} else {
		defer repo.Close()
		syncCheckpointPolicyForPrePush(ctx, repo, ps)
		if !checkpointPolicyAllowsGitHook(ctx, repo) {
			// Policy failures should skip checkpoint pushes, not abort the user's push.
			return nil
		}
	}

	// OPF pre-push rewrite: if OPF is configured, resolve the user's
	// decision (env > settings > prompt > non-TTY auto-run), then
	// re-redact unpushed v1 commits with the 8-layer pipeline before
	// pushing. Skipped entirely when OPF is off, so the common-case
	// fast path is unchanged.
	if redact.OPFEnabled() {
		cfg, _ := settings.Load(ctx) //nolint:errcheck // Load already failed at hook init; fall back to nil
		var opfCfg *settings.OPFSettings
		if cfg != nil && cfg.Redaction != nil {
			opfCfg = cfg.Redaction.OpenAIPrivacyFilter
		}
		decision, decisionErr := resolveOPFDecisionForPrePush(ctx, opfCfg, opfPrePushProgressWriter)
		if decisionErr != nil {
			logging.Warn(ctx, "OPF pre-push decision failed; aborting push",
				slog.String("error", decisionErr.Error()),
			)
			return decisionErr
		}
		switch decision {
		case OPFAbort:
			return errOPFAbortedByUser
		case OPFSkip:
			// User opted out for this push (or settings/env say
			// "never"). Push 7-layer content as-is.
			logging.Info(ctx, "OPF skipped for this push (user choice or settings)")
		case OPFRun:
			_, opfSpan := perf.Start(ctx, "opf_pre_push_rewrite")
			if repoErr != nil {
				opfSpan.RecordError(repoErr)
				opfSpan.End()
				logging.Warn(ctx, "OPF pre-push: failed to open repo; aborting push",
					slog.String("error", repoErr.Error()),
				)
				return repoErr
			}
			if _, rewriteErr := RewriteUnpushedV1WithOPF(ctx, repo, ps.pushTarget()); rewriteErr != nil {
				opfSpan.RecordError(rewriteErr)
				opfSpan.End()
				logging.Warn(ctx, "OPF pre-push rewrite failed; aborting push",
					slog.String("error", rewriteErr.Error()),
				)
				return rewriteErr
			}
			opfSpan.End()
		}
	}

	if deferAutomaticCheckpointPush {
		// Do this only after OPF has had a chance to rewrite v1: the outer
		// user push may explicitly include the metadata branch.
		logging.Info(ctx, "automatic checkpoint push deferred until a normal remote branch exists",
			slog.String("remote", ps.remote),
		)
		return nil
	}

	// Thread the span's context into the push so the network push and any
	// fetch+rebase recovery nest beneath it as child steps in the perf trace.
	pushCtx, pushCheckpointsSpan := perf.Start(ctx, "push_checkpoint_refs")
	for _, ref := range refs.Push {
		if err := pushRefIfNeeded(pushCtx, ps.pushTarget(), ref); err != nil {
			pushCheckpointsSpan.RecordError(err)
			pushCheckpointsSpan.End()
			return err
		}
	}
	pushCheckpointsSpan.End()

	cleanupPushedShadowBranches(ctx)
	return nil
}

// deferCheckpointPushOnEmptyRemote keeps Entire's metadata from becoming the
// first branch on a repository. Hosting providers such as GitHub make the first
// branch a repository's default, so a pre-push hook must not independently
// publish checkpoint metadata to a remote that has no branches yet: the user's
// own branch, pushed by the same git invocation right after this hook, must be
// the one to land first.
//
// The guard triggers only for a genuinely empty push target (no refs/heads/*).
// Once any branch exists there — including a checkpoint branch already present
// from an earlier push or a separate setup — our push can no longer be the one
// that establishes the default branch, so deferring would only block legitimate
// checkpoint syncs (e.g. a non-fast-forward v1 update) without preventing any
// harm.
//
// A separate checkpoint remote is intentionally exempt: it is a dedicated
// metadata store, rather than the repository the user is pushing to.
func deferCheckpointPushOnEmptyRemote(ctx context.Context, ps pushSettings) bool {
	if ps.hasCheckpointURL() {
		return false
	}

	dir, err := paths.WorktreeRoot(ctx)
	if err != nil {
		logging.Warn(ctx, "checkpoint push deferred: could not resolve worktree root",
			slog.String("remote", ps.remote),
			slog.String("error", err.Error()),
		)
		return true
	}

	targets, err := checkpointremote.PushTargetsInDir(ctx, dir, ps.remote)
	if err != nil {
		// Fail closed for checkpoint publication: the user's git push continues
		// normally, while a later push can publish the pending metadata once the
		// remote is reachable and has a branch.
		logging.Warn(ctx, "checkpoint push deferred: could not inspect remote branches",
			slog.String("remote", ps.remote),
			slog.String("error", err.Error()),
		)
		return true
	}

	// The empty-remote hazard exists only during the first-push window. Once
	// every push target has been observed with at least one branch we record a
	// fingerprint of that target set locally, so subsequent pushes short-circuit
	// here instead of paying an ls-remote network round trip on every push
	// forever. The fingerprint self-invalidates if the push URLs change.
	fingerprint := pushTargetsFingerprint(targets)
	if readPushBootstrapMarker(ctx) == fingerprint {
		return false
	}

	for _, target := range targets {
		out, lsErr := checkpointremote.LsRemoteInDir(ctx, dir, target, "refs/heads/*")
		if lsErr != nil {
			// Fail closed for checkpoint publication: the user's git push continues
			// normally, while a later push can publish the pending metadata once the
			// remote is reachable and has a branch.
			logging.Warn(ctx, "checkpoint push deferred: could not inspect remote branches",
				slog.String("remote", ps.remote),
				slog.String("target", target),
				slog.String("error", lsErr.Error()),
			)
			return true
		}

		// A truly empty target (no heads) is the only case our metadata push
		// could make the repository's default branch. Any existing head means
		// it is safe to publish now.
		if strings.TrimSpace(string(out)) == "" {
			logging.Info(ctx, "checkpoint push deferred until the remote has a branch",
				slog.String("remote", ps.remote),
				slog.String("target", target),
			)
			return true
		}
	}

	// Every push target now has a branch; remember it so the network probe above
	// is skipped on future pushes for this target set.
	writePushBootstrapMarker(ctx, fingerprint)
	return false
}

// pushTargetsFingerprint returns a stable, order-independent digest of the push
// targets. Hashing keeps the stored value bounded and avoids writing a push URL
// (which can embed credentials) verbatim to the marker.
func pushTargetsFingerprint(targets []string) string {
	sorted := append([]string(nil), targets...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

// pushBootstrapMarkerPath is the repo-level file recording that every resolved
// push target has been observed to carry at least one branch. It lives under
// the git common dir (shared across worktrees) rather than in .git/config so it
// never pollutes the user's git configuration.
func pushBootstrapMarkerPath(ctx context.Context) (string, error) {
	commonDir, err := GetGitCommonDir(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(commonDir, "entire", "checkpoint-push-bootstrap"), nil
}

// readPushBootstrapMarker returns the stored fingerprint, or "" if the marker is
// absent or unreadable.
func readPushBootstrapMarker(ctx context.Context) string {
	path, err := pushBootstrapMarkerPath(ctx)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is git common dir + constant, not user input
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writePushBootstrapMarker records fingerprint. Best-effort: a failure only
// means the next push re-runs the (correct) network probe, so it warns rather
// than surfacing an error into the push path.
func writePushBootstrapMarker(ctx context.Context, fingerprint string) {
	path, err := pushBootstrapMarkerPath(ctx)
	if err != nil {
		logging.Warn(ctx, "failed to resolve checkpoint push bootstrap marker path",
			slog.String("error", err.Error()),
		)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		logging.Warn(ctx, "failed to create checkpoint push bootstrap marker dir",
			slog.String("error", err.Error()),
		)
		return
	}
	if err := os.WriteFile(path, []byte(fingerprint+"\n"), 0o600); err != nil {
		logging.Warn(ctx, "failed to persist checkpoint push bootstrap marker",
			slog.String("error", err.Error()),
		)
	}
}

// prePushCheckpointRefs drains the per-checkpoint push queue and batch-pushes the
// recorded refs fast-forward-only (git-refs primary; never a force push — a
// diverged ref is recovered via fetch+replay). Transient push failures are logged and
// swallowed — like the v1 path, they must not block the user's git push — and the
// refs stay queued for the next pre-push. OPF is not applied (it is descoped for
// the git-refs store for now).
//
// It honors the checkpoint policy exactly like the v1 path: the policy gates on
// checkpoint *format* compatibility (diverged from the remote, or an unsupported
// local format), which is independent of the storage backend, so a blocked
// policy skips the ref push (leaving refs queued) rather than pushing.
func (s *ManualCommitStrategy) prePushCheckpointRefs(ctx context.Context, ps pushSettings) error {
	repo, err := OpenRepository(ctx)
	if err != nil {
		logging.Warn(ctx, "git-refs pre-push: open repo failed; skipping checkpoint push",
			slog.String("error", err.Error()))
		return nil
	}
	defer repo.Close()

	// Refresh the checkpoint policy from the remote, then skip the ref push
	// (leaving refs queued) if the policy is diverged or the local format is
	// unsupported — same gate the v1 path uses.
	syncCheckpointPolicyForPrePush(ctx, repo, ps)
	if !checkpointPolicyAllowsGitHook(ctx, repo) {
		return nil
	}

	if _, err := flushCheckpointRefsQueue(ctx, repo, ps.pushTarget()); err != nil {
		// Fail-soft: a checkpoint-ref push failure must never block the user's
		// git push. The refs stay queued for the next pre-push.
		logging.Warn(ctx, "git-refs pre-push: checkpoint ref push failed; refs left queued",
			slog.String("error", err.Error()))
	}

	cleanupPushedShadowBranches(ctx)
	return nil
}

// PushQueuedCheckpointRefs pushes any queued checkpoint refs to the configured
// checkpoint remote, surfacing errors (unlike the fail-soft pre-push path); the
// caller owns the repo. It returns the number of refs pushed and whether
// pushing is disabled in settings — a distinct signal from pushed==0 with
// pushing enabled (an empty queue), so callers can report the two accurately.
// Like the pre-push paths, a checkpoint policy that blocks pushing errors with
// the refs left queued. Currently used by the checkpoint migration command's
// opt-in "push now".
func PushQueuedCheckpointRefs(ctx context.Context, repo *git.Repository, remote string) (pushed int, pushDisabled bool, err error) {
	ps := resolvePushSettings(ctx, remote)
	if ps.pushDisabled {
		return 0, true, nil
	}
	syncCheckpointPolicyForPrePush(ctx, repo, ps)
	if !checkpointPolicyAllowsGitHook(ctx, repo) {
		return 0, false, errors.New("checkpoint policy does not allow pushing checkpoint refs; refs stay queued")
	}
	pushed, err = flushCheckpointRefsQueue(ctx, repo, ps.pushTarget())
	// Clean up even on a partial/failed flush: a diverged batch can push some
	// refs and still return an error, and the shadow branches for the refs that
	// *did* land must still be cleaned up — parity with the pre-push path, which
	// always runs cleanup after flush regardless of its error.
	cleanupPushedShadowBranches(ctx)
	return pushed, false, err
}

// flushCheckpointRefsQueue drains the push-discovery queue and batch-pushes the
// recorded refs fast-forward-only, recovering a diverged ref by fetch+replay and
// removing from the queue only the refs that land. It returns the number pushed.
//
// Shared by the git-refs pre-push path (which logs and ignores the error to
// never block the user's push) and the migration command's opt-in push (which
// surfaces it). Stale entries — refs no longer present locally — are pruned so
// they don't block the queue forever.
func flushCheckpointRefsQueue(ctx context.Context, repo *git.Repository, pushTarget string) (int, error) {
	queue, err := checkpoint.PushQueueForRepo(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("resolve push queue: %w", err)
	}
	queued, err := queue.Drain()
	if err != nil {
		return 0, fmt.Errorf("drain push queue: %w", err)
	}
	if len(queued) == 0 {
		return 0, nil
	}

	pushCtx, pushSpan := perf.Start(ctx, "push_checkpoint_refs")
	defer pushSpan.End()

	existing, stale := partitionLocalRefs(repo, queued)
	if len(stale) > 0 {
		if err := queue.Remove(stale); err != nil {
			logging.Warn(ctx, "git-refs push: prune stale queue entries failed",
				slog.String("error", err.Error()))
		}
	}
	if len(existing) == 0 {
		return 0, nil
	}

	// Progress: pushing many refs over the network can take tens of seconds, so
	// surface it (matching the v1 path's "[entire] Pushing ..." line) instead of
	// leaving the user's git push apparently hung. Written to stderr, which git
	// shows during the pre-push hook.
	displayTarget := displayPushTarget(pushTarget)
	fmt.Fprintf(os.Stderr, "[entire] Pushing %d checkpoint ref(s) to %s...", len(existing), displayTarget)
	stop := startProgressDots(os.Stderr)

	// Fast path: push all refs in one round-trip (fast-forward-only). If every
	// ref was up to date or fast-forwarded, we're done.
	if err := batchPushRefs(pushCtx, pushTarget, existing); err == nil {
		stop(" done")
		if removeErr := queue.Remove(existing); removeErr != nil {
			logging.Warn(ctx, "git-refs push: clear pushed refs from queue failed",
				slog.String("error", removeErr.Error()))
		}
		return len(existing), nil
	}
	stop("")

	// At least one ref was rejected — typically a non-fast-forward divergence
	// (the same checkpoint re-written on another machine). Retry per ref with
	// fetch+replay recovery, and remove from the queue only the refs that land
	// (a genuine cherry-pick conflict leaves that ref queued for a later push,
	// never force-overwriting the remote).
	fmt.Fprintf(os.Stderr, "[entire] Some checkpoint refs diverged; syncing %d ref(s) individually...", len(existing))
	stop = startProgressDots(os.Stderr)
	pushed := make([]plumbing.ReferenceName, 0, len(existing))
	var firstErr error
	for _, ref := range existing {
		if err := pushCheckpointRefWithRecovery(pushCtx, pushTarget, ref); err != nil {
			logging.Warn(ctx, "git-refs push: checkpoint ref push/sync failed; left queued, not overwritten",
				slog.String("ref", ref.String()), slog.String("error", err.Error()))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		pushed = append(pushed, ref)
	}
	stop(fmt.Sprintf(" pushed %d of %d", len(pushed), len(existing)))
	if err := queue.Remove(pushed); err != nil {
		logging.Warn(ctx, "git-refs push: clear pushed refs from queue failed",
			slog.String("error", err.Error()))
	}
	if firstErr != nil {
		return len(pushed), fmt.Errorf("%d of %d checkpoint refs failed to push: %w",
			len(existing)-len(pushed), len(existing), firstErr)
	}
	return len(pushed), nil
}

// cleanupPushedShadowBranches runs post-push shadow-branch cleanup. Failures are
// non-fatal — shadow branches just accumulate until `entire clean` or the next
// successful push.
func cleanupPushedShadowBranches(ctx context.Context) {
	if deleted, cleanupErr := CleanupPushedShadowBranches(ctx); cleanupErr != nil {
		logging.Warn(ctx, "post-push shadow branch cleanup failed",
			slog.String("error", cleanupErr.Error()),
		)
	} else if deleted > 0 {
		logging.Info(ctx, "cleaned up vestigial shadow branches",
			slog.Int("count", deleted),
		)
	}
}
