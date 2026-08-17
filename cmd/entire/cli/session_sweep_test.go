package cli

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sweep predicate decides, from state files alone, whether a background
// sweep is worth spawning. False negatives mean zombies linger (the pre-sweep
// status quo); false positives spawn a process that no-ops — so the predicate
// leans conservative: fresh ENDED sessions are NOT zombies (PostCommit
// carry-forward must get its chance), and live sessions are never flagged.
func TestIsSweepableZombie(t *testing.T) {
	t.Parallel()

	now := time.Now()
	old := now.Add(-48 * time.Hour)
	fresh := now.Add(-1 * time.Hour)

	// Dead-owner fixture: a LIVE pid with a mismatched start-time fingerprint
	// reads as PID reuse → LivenessDead. Do NOT use a negative/absent PID —
	// proclive.Check returns LivenessUnknown for PID <= 0 and OwnerExited()
	// then reports false. Same fixture as session_finalize_test.go:34.
	deadOwner := &proclive.Identity{PID: os.Getpid(), Start: "bogus-start-fingerprint"}

	tests := []struct {
		name  string
		state session.State
		want  bool
	}{
		{
			name: "active with dead owner is a zombie",
			state: session.State{
				Phase: session.PhaseActive,
				Owner: deadOwner,
			},
			want: true,
		},
		{
			name: "active with no owner recorded is not a zombie (legacy state)",
			state: session.State{
				Phase: session.PhaseActive,
			},
			want: false,
		},
		{
			name: "ended uncondensed with steps, older than threshold, is a zombie",
			state: session.State{
				Phase:     session.PhaseEnded,
				StepCount: 3,
				EndedAt:   &old,
			},
			want: true,
		},
		{
			name: "ended uncondensed but fresh is NOT a zombie (carry-forward window)",
			state: session.State{
				Phase:     session.PhaseEnded,
				StepCount: 3,
				EndedAt:   &fresh,
			},
			want: false,
		},
		{
			name: "ended and fully condensed is not a zombie",
			state: session.State{
				Phase:          session.PhaseEnded,
				StepCount:      3,
				FullyCondensed: true,
				EndedAt:        &old,
			},
			want: false,
		},
		{
			name: "ended with zero steps is not a zombie (nothing to condense)",
			state: session.State{
				Phase:   session.PhaseEnded,
				EndedAt: &old,
			},
			want: false,
		},
		{
			name: "ended with nil EndedAt falls back to LastInteractionTime",
			state: session.State{
				Phase:               session.PhaseEnded,
				StepCount:           3,
				LastInteractionTime: &old,
			},
			want: true,
		},
		{
			name: "ended with no timestamps at all is skipped (cannot age-gate)",
			state: session.State{
				Phase:     session.PhaseEnded,
				StepCount: 3,
			},
			want: false,
		},
		{
			name: "idle session is never a zombie",
			state: session.State{
				Phase:     session.PhaseIdle,
				StepCount: 3,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isSweepableZombie(&tt.state, now))
		})
	}
}

// TestRunSessionSweep_CondensesOldEndedZombie_LeavesFreshAlone — the
// regression this whole feature exists for: an ENDED session with uncondensed
// checkpoints and a shadow branch used to linger until a human ran
// `entire doctor --force` (a real one sat for 4 days). The sweep must fix the
// old one and must NOT touch a freshly-ended one, whose PostCommit
// carry-forward window is still open.
func TestRunSessionSweep_CondensesOldEndedZombie_LeavesFreshAlone(t *testing.T) {
	// Cannot use t.Parallel() because t.Chdir modifies process-global state.
	dir := setupGitRepoForPhaseTest(t)
	t.Chdir(dir)
	ctx := context.Background()

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	createShadowBranchRef(t, repo, testBaseCommit, "")

	old := time.Now().Add(-48 * time.Hour)
	fresh := time.Now().Add(-time.Hour)

	zombie := &strategy.SessionState{
		SessionID:  "2026-08-17-sweep-old-zombie",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseEnded,
		StepCount:  2,
		StartedAt:  old.Add(-time.Hour),
		EndedAt:    &old,
	}
	require.NoError(t, strategy.SaveSessionState(ctx, zombie))

	recent := &strategy.SessionState{
		SessionID:  "2026-08-17-sweep-fresh-ended",
		BaseCommit: testBaseCommit,
		Phase:      session.PhaseEnded,
		StepCount:  2,
		StartedAt:  fresh.Add(-time.Hour),
		EndedAt:    &fresh,
	}
	require.NoError(t, strategy.SaveSessionState(ctx, recent))

	require.NoError(t, runSessionSweep(ctx))

	states, err := strategy.ListSessionStates(ctx)
	require.NoError(t, err)

	byID := map[string]*strategy.SessionState{}
	for _, st := range states {
		byID[st.SessionID] = st
	}

	// The fresh session must be exactly as we left it.
	freshAfter, ok := byID["2026-08-17-sweep-fresh-ended"]
	require.True(t, ok, "fresh ended session must survive the sweep untouched")
	assert.Equal(t, session.PhaseEnded, freshAfter.Phase)
	assert.False(t, freshAfter.FullyCondensed)
	assert.Equal(t, 2, freshAfter.StepCount)

	// The old zombie must no longer be flagged as a condensable zombie:
	// CondenseSessionByID either condenses it (state cleared or StepCount
	// reset) — assert via the same predicate the sweep uses.
	if zombieAfter, exists := byID["2026-08-17-sweep-old-zombie"]; exists {
		assert.False(t, strategy.IsCondensableEndedSession(repo, zombieAfter),
			"old zombie must not remain condensable after the sweep")
	}
	// (absence from the list is also success: fully cleaned up)
}

// sessionSweepNeeded is the hook-side spawn decision. It must be a pure
// function over the state list: SpawnDetached is untestable in-process (it
// no-ops under `go test`), so correctness of "would we spawn?" lives here.
func TestSessionSweepNeeded(t *testing.T) {
	t.Parallel()

	now := time.Now()
	old := now.Add(-48 * time.Hour)

	assert.False(t, sessionSweepNeeded(nil, now), "no sessions → no sweep")

	healthy := &session.State{Phase: session.PhaseIdle}
	assert.False(t, sessionSweepNeeded([]*session.State{healthy}, now))

	zombie := &session.State{Phase: session.PhaseEnded, StepCount: 1, EndedAt: &old}
	assert.True(t, sessionSweepNeeded([]*session.State{healthy, zombie}, now),
		"one zombie among healthy sessions → sweep")
}
