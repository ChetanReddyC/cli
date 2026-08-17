package cli

import (
	"time"

	"github.com/entireio/cli/cmd/entire/cli/session"
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
