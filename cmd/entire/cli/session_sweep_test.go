package cli

import (
	"os"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"

	"github.com/stretchr/testify/assert"
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
