package cli

import (
	"fmt"
	"io"

	"github.com/entireio/cli/cmd/entire/cli/agentimport"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

// newImportProgressReporter wires an agentimport.Progress to user-visible
// output on w for one agent's import run. On an interactive terminal
// (outside ACCESSIBLE mode) it drives an updatable spinner whose message
// tracks "Importing <agentName> sessions... (session i/N · turn j/M)";
// callers must call the returned stop exactly once when the run finishes —
// on both the success and error paths — so no spinner frame is left
// dangling to corrupt whatever prints next. Otherwise (non-TTY, piped, or
// ACCESSIBLE mode) it prints one plain, ANSI-free line per session from
// SessionStart, and stop is a no-op.
func newImportProgressReporter(w io.Writer, agentName string) (progress *agentimport.Progress, stop func(success bool)) {
	if !interactive.IsTerminalWriter(w) || IsAccessibleMode() {
		return &agentimport.Progress{
			SessionStart: func(sessionIndex, sessionTotal int, _, _ string, turnCount int) {
				fmt.Fprintf(w, "Importing %s session %d/%d (%d %s)...\n",
					agentName, sessionIndex+1, sessionTotal, turnCount, pluralize("turn", turnCount))
			},
		}, func(bool) {}
	}

	update, spinnerStop := startUpdatableSpinner(w, fmt.Sprintf("Importing %s sessions...", agentName))
	var curSession, curSessionTotal, curTurnTotal int
	render := func(turnsDone int) {
		update(fmt.Sprintf("Importing %s sessions... (session %d/%d · turn %d/%d)",
			agentName, curSession, curSessionTotal, turnsDone, curTurnTotal))
	}
	advance := func(_, turnIndex, _ int) {
		render(turnIndex + 1)
	}
	progress = &agentimport.Progress{
		SessionStart: func(sessionIndex, sessionTotal int, _, _ string, turnCount int) {
			curSession, curSessionTotal, curTurnTotal = sessionIndex+1, sessionTotal, turnCount
			render(0)
		},
		// TurnWritten and TurnSkipped share the same advance path: the
		// counter must sweep to turnCount/turnCount regardless of *why* a
		// turn didn't need writing (already imported, or DryRun), otherwise
		// a fully-skipped or dry-run session's completion line would freeze
		// at "turn 0/M".
		TurnWritten: advance,
		TurnSkipped: advance,
	}
	return progress, spinnerStop
}
