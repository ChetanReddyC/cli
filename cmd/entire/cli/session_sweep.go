package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
)

// endedSessionSweepAge is how long an ENDED-but-uncondensed session must sit
// before the background sweep may condense it. Condensing early would forfeit
// PostCommit's per-commit carry-forward linkage for sessions whose user is
// still about to commit — CondenseAndMarkFullyCondensed deliberately skips
// FilesTouched sessions for exactly that reason. 24h matches
// activeSessionInteractionThreshold: after a day with no commit, the linkage
// is clearly not coming and the session is just O(N) drag on every commit.
const endedSessionSweepAge = 24 * time.Hour

// isSweepableZombie reports whether the state marks this session as safe for
// the background sweep to fix. It deliberately needs no repository access —
// the priciest thing it does is OwnerExited()'s per-active-state process
// probe, which the `entire status` path already pays per state — so the
// session-start hook caller stays cheap. The sweep itself re-validates
// before acting (see runSessionSweep's safety notes).
//
// Condense-only contract: ENDED sessions whose steps turn out to have no
// shadow branch are doctor's discard case, filtered later — this
// predicate only ever nominates sessions, it never acts.
func isSweepableZombie(st *session.State, now time.Time) bool {
	if st.Phase.IsActive() {
		return st.OwnerExited()
	}
	if st.Phase != session.PhaseEnded || st.FullyCondensed || st.StepCount <= 0 {
		return false
	}
	ref := st.EndedAt
	if ref == nil {
		ref = st.LastInteractionTime
	}
	if ref == nil {
		return false // legacy state without timestamps: leave it to doctor
	}
	return now.Sub(*ref) >= endedSessionSweepAge
}

// runSessionSweep is the detached background pass that fixes zombie sessions:
// ACTIVE sessions whose owning agent process is gone are finalized exactly as
// a clean SessionStop would (finalizeExitedSessions, which re-validates
// OwnerExited under the per-session lock), and ENDED sessions past the
// carry-forward window with condensable data are condensed via the same
// engine `entire doctor --force` uses.
//
// Safety contract (honest version — mirror this in the PR description):
// condense-only, enforced by OUR pre-checks, not by the engine.
// CondenseSessionByID's locked closure re-checks only shadow-branch
// existence; we therefore re-load each candidate and re-run the full zombie
// predicate immediately before condensing. That narrows — does not close —
// the window in which a resumed (ENDED→ACTIVE) session gets condensed
// anyway: acceptable, because the precondition is >24h idle (a seconds-wide
// race against a day-old zombie) and a condense of a just-resumed session is
// coherent. If the shadow branch vanishes between our check and the engine's
// lock, the engine clears the state — correct cleanup, since that only
// happens when a concurrent condense already succeeded. Everything is
// best-effort: a failed session is logged and retried by the next sweep.
func runSessionSweep(ctx context.Context) error {
	logCtx := logging.WithComponent(ctx, "session-sweep")

	states, err := strategy.ListSessionStates(ctx)
	if err != nil {
		return fmt.Errorf("failed to list session states: %w", err)
	}

	// Active sessions with a dead owner: finalize + eager condense. This also
	// refreshes the matched entries in `states` from disk.
	if n := finalizeExitedSessions(ctx, states); n > 0 {
		logging.Info(logCtx, "sweep finalized exited sessions", slog.Int("count", n))
	}

	repo, err := openRepository(ctx)
	if err != nil {
		return err
	}
	defer repo.Close()

	store, err := session.NewStateStore(ctx)
	if err != nil {
		return fmt.Errorf("failed to open session state store: %w", err)
	}

	now := time.Now()
	for _, st := range states {
		if st.Phase != session.PhaseEnded || !isSweepableZombie(st, now) {
			continue
		}
		// Re-load and re-check right before acting: the snapshot may be stale
		// (a resumed session flips ENDED→ACTIVE; a concurrent sweep may have
		// condensed it). See the safety contract above for the residual race.
		fresh, lerr := store.Load(ctx, st.SessionID)
		if lerr != nil || fresh == nil {
			continue
		}
		if fresh.Phase != session.PhaseEnded || !isSweepableZombie(fresh, now) {
			continue
		}
		if !strategy.IsCondensableEndedSession(repo, fresh) {
			continue // no shadow branch → doctor's discard case, never automatic
		}
		if condErr := GetStrategy(ctx).CondenseSessionByID(ctx, fresh.SessionID); condErr != nil {
			logging.Warn(logCtx, "sweep condense failed",
				slog.String("session_id", fresh.SessionID),
				slog.String("error", condErr.Error()))
			continue
		}
		logging.Info(logCtx, "sweep condensed ended zombie session",
			slog.String("session_id", fresh.SessionID))
	}
	return nil
}
