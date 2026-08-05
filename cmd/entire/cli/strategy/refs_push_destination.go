package strategy

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/remote"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// refsPushDestination is where checkpoint refs are pushed, plus how to name that
// place in user-facing output.
type refsPushDestination struct {
	// target is passed to git push / the recovery fetch: a remote name, or a URL.
	target string
	// display names the destination in progress and warning output.
	display string
	// checkpointRemote is true when target came from a configured
	// checkpoint_remote, which gates the "a checkpoint remote is configured"
	// hint — that hint must not fire for a URL we picked ourselves.
	checkpointRemote bool
	// ignoredPushURLs counts push URLs of a multi-URL remote that will NOT
	// receive checkpoint refs. Zero in every single-destination topology.
	ignoredPushURLs int
}

// resolveRefsPushDestination picks the single destination for checkpoint-ref
// pushes.
//
// Checkpoint refs need ONE deterministic destination, because the push-discovery
// queue records only a ref (`{"ref": …}`) with no per-destination state: a ref is
// removed from the queue once "the push" succeeds, so "the push" has to mean one
// place. Relying on git's fan-out across a remote's several push URLs breaks that
// in both directions — a single failing URL fails the whole invocation and no ref
// unqueues even though some URLs took it, and an unreachable FIRST URL makes git
// die() before it reaches any later URL at all.
//
// So when a remote carries more than one push URL we target its first push URL
// directly (the one git itself would push to first) and ignore the rest. The
// recovery fetch in fetchAndRebaseRefCommon uses the same target, so — unlike the
// fan-out path, which reconciled the remote's FETCH url while pushing to its
// pushurls — the URL we reconcile is finally the URL we push to.
//
// Consequences, deliberately accepted:
//   - Checkpoint refs live in exactly one repository. Cloning that repository
//     resolves them (its url becomes the clone's fetch URL); cloning a different
//     mirror of the same code does not. Mirroring checkpoints to several
//     repositories is what checkpoint_remote is for.
//   - A first push URL that REJECTS a ref (non-fast-forward) no longer lets later
//     URLs receive it. git would have carried on to them; we stop. That is the
//     price of a deterministic destination, and it is the case the queue can
//     actually reason about.
//
// The git-branch backend deliberately keeps git's fan-out: its v1 branch is a
// single shared ref with no queue to keep coherent, and mirroring it to every
// push URL is behavior users configure their remotes for.
//
// A single push URL keeps the remote NAME as the target rather than resolving it
// to a URL, so the overwhelmingly common topology behaves exactly as before —
// remote-tracking refs still update, output still says "origin", and no
// URL-keyed promisor config appears.
func resolveRefsPushDestination(ctx context.Context, ps pushSettings) refsPushDestination {
	target := ps.pushTarget()

	// A configured checkpoint_remote is already a single explicit destination.
	if ps.hasCheckpointURL() {
		return refsPushDestination{target: target, display: displayPushTarget(target), checkpointRemote: true}
	}

	// Pushing straight to a URL (git passes a bare URL through to the hook) is
	// likewise already single-destination.
	if remote.IsURL(target) {
		return refsPushDestination{target: target, display: displayPushTarget(target)}
	}

	urls, err := remote.GetPushURLs(ctx, target)
	if err != nil {
		// Not a configured remote, or git could not report its URLs. Keep the
		// target as given; the push itself will report any real problem.
		logging.Debug(ctx, "git-refs push: could not enumerate push URLs; using target as given",
			slog.String("target", target),
			slog.String("error", err.Error()),
		)
		return refsPushDestination{target: target, display: target}
	}
	if len(urls) < 2 {
		return refsPushDestination{target: target, display: target}
	}

	return refsPushDestination{
		target:          urls[0],
		display:         fmt.Sprintf("%s (first of %d push URLs)", displayURL(urls[0]), len(urls)),
		ignoredPushURLs: len(urls) - 1,
	}
}

// displayURL renders a push destination for humans with any credentials removed.
// RedactURL is only safe for URL-shaped values: given a plain filesystem path it
// parses to an empty scheme and host and renders as ":///path/to/repo". Paths
// carry no credentials, so pass them through unchanged.
func displayURL(u string) string {
	if remote.IsURL(u) {
		return remote.RedactURL(u)
	}
	return u
}

// warnIgnoredPushURLs tells the user, once per push, that checkpoint refs are
// going to one URL of a multi-URL remote — otherwise the choice is invisible and
// looks like the other mirrors silently lost their checkpoints.
func (d refsPushDestination) warnIgnoredPushURLs(ctx context.Context, errOut io.Writer) {
	if d.ignoredPushURLs == 0 {
		return
	}
	fmt.Fprintf(errOut, "[entire] Checkpoints go to one repository: %s. %d other push URL(s) of this remote will not receive them.\n",
		d.display, d.ignoredPushURLs)
	fmt.Fprintln(errOut, "[entire] To store checkpoints in a specific repository instead, set checkpoint_remote in .entire/settings.json.")
	logging.Info(ctx, "git-refs push: multi-URL remote, pushing checkpoint refs to the first push URL only",
		slog.String("target", displayURL(d.target)),
		slog.Int("ignored_push_urls", d.ignoredPushURLs),
	)
}
