//go:build integration

package integration

import (
	"os"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
)

// TestSubagentCheckpoints_CommittedMidTurn_LeavesNoShadowBranch reproduces the
// orphaned shadow branch behind the e2e failure of
// TestSingleSessionSubagentCommitInTurn.
//
// The subagent writes a file and commits it itself, mid-turn. That commit condenses
// the session and deletes the shadow branch. post-task then fires with nothing left
// to snapshot — the file is already in HEAD — so it must skip the task checkpoint.
// Creating one instead mints a *new* shadow branch after condensation has already
// run, and nothing ever condenses it away: turn-end sees no file modifications and
// skips, so the branch outlives the session.
//
// The trap is that the subagent's transcript still records the Write. Deciding from
// the transcript alone conflates "the subagent wrote this at some point" with "there
// is an uncommitted change here" — see filterToUncommittedFiles, which the turn-end
// path already applies for exactly this reason.
func TestSubagentCheckpoints_CommittedMidTurn_LeavesNoShadowBranch(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("use a subagent to write docs/red.md and commit it", nil)

	const (
		taskToolUseID = "toolu_01CommitInTurn"
		subagentID    = "a0011223344556677"
		editedFile    = "docs/red.md"
	)
	// The subagent's own transcript records the Write; the main transcript does not.
	session.CreateSubagentTranscript(subagentID, []FileChange{{Path: editedFile, Content: "Red is warm.\n"}})

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// The subagent writes the file and commits it itself, still inside the turn.
	env.WriteFile(editedFile, "Red is a warm colour.\n")
	env.GitCommitWithShadowHooksAsAgent("Add red.md", editedFile)

	// Condensation ran on that commit and cleaned up the shadow branch.
	if got := shadowBranches(env); len(got) != 0 {
		t.Fatalf("precondition: shadow branch should be gone after the mid-turn commit, got %v", got)
	}

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	if got := shadowBranches(env); len(got) != 0 {
		t.Errorf("post-task created a shadow branch for already-committed work: %v\n"+
			"nothing will condense it away — turn-end skips when no files changed", got)
	}
}

// TestSubagentCheckpoints_UncommittedWork_StillCheckpoints is the companion guard:
// filtering already-committed paths must not stop a subagent whose work is still
// uncommitted from getting its task checkpoint.
func TestSubagentCheckpoints_UncommittedWork_StillCheckpoints(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("use a subagent to write docs/blue.md", nil)

	const (
		taskToolUseID = "toolu_01UncommittedWork"
		subagentID    = "a7766554433221100"
		editedFile    = "docs/blue.md"
	)
	session.CreateSubagentTranscript(subagentID, []FileChange{{Path: editedFile, Content: "Blue is cool.\n"}})

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Left uncommitted, unlike the test above.
	env.WriteFile(editedFile, "Blue is a cool colour.\n")

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	wantPath := ".entire/metadata/" + session.ID + "/tasks/" + taskToolUseID + "/checkpoint.json"
	if !env.FileExistsInBranch(env.GetShadowBranchName(), wantPath) {
		t.Errorf("task checkpoint missing for uncommitted subagent work (%s)", wantPath)
	}
}

// shadowBranches returns the per-base-commit shadow branches, excluding the
// permanent committed-checkpoint branch which is not session-scoped.
func shadowBranches(env *TestEnv) []string {
	var out []string
	for _, b := range env.ListBranchesWithPrefix("entire/") {
		if b == paths.MetadataBranchName {
			continue
		}
		out = append(out, b)
	}
	return out
}

// TestSubagentCheckpoints_CommitWhileIdleWithLiveMarker_LinksAndCondensesContent
// reproduces the incident this PR fixes: a background subagent commits (or has
// its work committed) between the parent session's turns, while the parent is
// IDLE. Before this PR, tryAgentCommitFastPath only trusted ACTIVE sessions, so
// the fast path declined; the slow, content-detection path then found nothing
// to link (the subagent's work wasn't in FilesTouched yet, since no capture had
// run for it), and the commit shipped with no Entire-Checkpoint trailer at all —
// six of seven commits on a real subagent-driven branch went unlinked this way.
//
// A first, already-completed background task seeds a shadow branch before the
// task under test ever launches — mirroring the real incident, where the
// unlinked commits land well into an already-active session, not on its very
// first turn. This is load-bearing, not incidental: findSessionsForWorktree's
// listAllSessionStates prunes any IDLE session with neither a shadow branch nor
// a LastCheckpointID as an orphaned pre-state-machine record (manual_commit_session.go).
// A session whose only activity so far is one open in-flight marker — the exact
// shape this PR's idle+marker eligibility targets — has neither, and gets swept
// before prepare-commit-msg ever sees it. That sweep is unrelated to this PR
// (it predates PR #2032) and out of scope for a test; the seed task sidesteps
// it the way a real multi-turn session would, staying read-only so it never
// touches FilesTouched or the session's condensed-transcript baseline.
//
// This drives the REAL prepare-commit-msg and post-commit git hooks end to end
// (GitCommitWithShadowHooksAsAgent), so it is the arbiter of whether the trailer
// tryAgentCommitFastPath adds (idle+marker eligibility, manual_commit_hooks.go)
// is actually backed by content: post-commit's headHasCheckpointTrailer gate
// only runs the commit-snapshot capture (captureInFlightTasksForCommit,
// lifecycle.go) when the trailer landed, and it runs before
// strategy.PostCommit's condensation in the same hook invocation. The
// marker's LastCapturedTranscriptBytes assertion below is the direct proof
// that capture ran; the checkpoint-content assertion after it proves the
// trailer resolves to something real rather than dangling — reachable here
// only because idleWithLiveMarker (Task 1) bypasses shouldCondenseWithOverlapCheck's
// overlap requirement for this IDLE, marker-bearing session (its FilesTouched
// carries no evidence tying it to editedFile).
func TestSubagentCheckpoints_CommitWhileIdleWithLiveMarker_LinksAndCondensesContent(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	sess.CreateTranscript("delegate two background tasks", nil)

	const (
		seedToolUseID = "toolu_01IdleMarkerSeed"
		seedAgentID   = "e5555666677778888"
		taskToolUseID = "toolu_01IdleMarkerCommit"
		subagentID    = "d4444555566667777"
		editedFile    = "docs/idlemarker.md"
	)

	// Real Claude Code always sends a transcript_path on UserPromptSubmit;
	// it is what populates the persisted SessionState.TranscriptPath that
	// captureInFlightTaskCommitSnapshot later reads at post-commit time (no
	// hook payload is available there to supply it directly, unlike
	// SubagentStop's agent_transcript_path).
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Seed task: a read-only background subagent that finishes within this
	// same turn. Its SubagentStop Final capture (bypassNoChangesSkip) writes a
	// zero-file task step, which creates the shadow branch and bumps StepCount
	// without ever touching FilesTouched.
	if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, seedToolUseID); err != nil {
		t.Fatalf("SimulatePreTask (seed) failed: %v", err)
	}
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:       sess.ID,
		TranscriptPath:  sess.TranscriptPath,
		ToolUseID:       seedToolUseID,
		AgentID:         seedAgentID,
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (seed background stub) failed: %v", err)
	}
	seedTranscriptPath := sess.CreateSubagentTranscript(seedAgentID, nil)
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:           sess.ID,
		TranscriptPath:      sess.TranscriptPath,
		AgentID:             seedAgentID,
		AgentTranscriptPath: seedTranscriptPath,
		ToolUseID:           seedToolUseID,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop (seed) failed: %v", err)
	}

	// The task under test launches in the same (still-ACTIVE) turn.
	if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Background launch: marker recorded while the parent is still ACTIVE
	// (mid-turn).
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:       sess.ID,
		TranscriptPath:  sess.TranscriptPath,
		ToolUseID:       taskToolUseID,
		AgentID:         subagentID,
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
	}

	// Turn ends: the parent goes IDLE while the background subagent keeps
	// running. The subagent's transcript doesn't exist yet, so this turn-end's
	// incremental backstop (captureInFlightTaskIncremental) has nothing to
	// snapshot — the marker survives untouched, and the parent session
	// carries no FilesTouched yet. This is the shape idleWithLiveMarker exists
	// for: an IDLE session whose background task is still genuinely live.
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if hasInFlightTask(state, seedToolUseID) {
		t.Fatalf("seed task's marker should have been claimed by its own subagent-stop, state=%+v", state)
	}
	if state == nil || state.Phase != session.PhaseIdle {
		t.Fatalf("expected session to be IDLE after turn-end, got %+v", state)
	}
	if !hasInFlightTask(state, taskToolUseID) {
		t.Fatalf("expected in-flight marker to survive turn-end, state=%+v", state)
	}

	// The subagent does its actual work while the parent sits idle between
	// turns: a realistic transcript (the real Claude Code transcript
	// analyzer, not a stub) plus the resulting file.
	subagentTranscriptPath := sess.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: editedFile, Content: "# Idle marker\n"},
	})
	env.WriteFile(editedFile, "# Idle marker\n\nWritten by a background subagent while the parent is idle.\n")

	// The commit lands while the session is IDLE, through the real
	// prepare-commit-msg + post-commit hook chain, with no TTY (agent-mode
	// commit) — the exact shape of the incident.
	env.GitCommitWithShadowHooksAsAgent("Add idle-marker doc", editedFile)

	headHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(headHash)
	if checkpointID == "" {
		t.Fatalf("commit made while idle with a live marker should carry an Entire-Checkpoint trailer")
	}

	// Direct proof that captureInFlightTaskCommitSnapshot ran inside this
	// post-commit invocation, before condensation: it is the only code path
	// that advances a marker's LastCapturedTranscriptBytes outside of the
	// turn-end backstop (which had nothing to capture yet — the subagent
	// transcript didn't exist at Stop time), and it only runs at all because
	// headHasCheckpointTrailer saw the trailer this same prepare-commit-msg
	// just added. This is the ordering unit tests can't observe end to end.
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	marker := state.FindInFlightTask(taskToolUseID)
	if marker == nil {
		t.Fatalf("expected marker %s to still exist after the commit", taskToolUseID)
	}
	subagentTranscriptInfo, statErr := os.Stat(subagentTranscriptPath)
	if statErr != nil {
		t.Fatalf("failed to stat subagent transcript: %v", statErr)
	}
	if marker.LastCapturedTranscriptBytes != subagentTranscriptInfo.Size() {
		t.Errorf("commit-snapshot capture did not run: marker.LastCapturedTranscriptBytes = %d, want %d (subagent transcript size)",
			marker.LastCapturedTranscriptBytes, subagentTranscriptInfo.Size())
	}

	// The condensed permanent checkpoint is contentful, not a dangling
	// trailer: it resolves at all (rather than PostCommit skipping this IDLE
	// session for lack of overlap evidence) only because idleWithLiveMarker
	// bypasses the overlap check, and it carries editedFile in FilesTouched
	// via CondenseSession's committed-files fallback (filterFilesTouched in
	// manual_commit_condensation.go).
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:              checkpointID,
		SessionID:                 sess.ID,
		FilesTouched:              []string{editedFile},
		ExpectedTranscriptContent: []string{"delegate two background tasks"},
	})

	// The marker survives condensation: the task is still running, and
	// SubagentStop (not this commit) remains the authoritative Final capture
	// that claims it. This is the invariant the unit tests leave unpinned.
	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || !hasInFlightTask(state, taskToolUseID) {
		t.Fatalf("expected in-flight marker for %s to survive condensation, state=%+v", taskToolUseID, state)
	}

	// The subagent now finishes: the real completion signal runs the Final
	// capture and claims the marker.
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:           sess.ID,
		TranscriptPath:      sess.TranscriptPath,
		AgentID:             subagentID,
		AgentTranscriptPath: subagentTranscriptPath,
		ToolUseID:           taskToolUseID,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop failed: %v", err)
	}

	state, err = env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state != nil && hasInFlightTask(state, taskToolUseID) {
		t.Errorf("in-flight marker for %s should be cleared after subagent-stop", taskToolUseID)
	}
	// The already-committed file is not re-added as a tracked file: the
	// analyzer's ModifiedFiles included it, but filterToUncommittedFiles
	// strips anything already in HEAD before it ever reaches FilesTouched.
	if state != nil {
		for _, f := range state.FilesTouched {
			if f == editedFile {
				t.Errorf("committed file %s should not be re-added to FilesTouched by the Final capture", editedFile)
			}
		}
	}

	// The Final capture re-stores the subagent's transcript unconditionally
	// (no growth dedup on the Final path, by design) into the new task
	// checkpoint on the (freshly re-created) shadow branch for the new HEAD.
	shadowBranch := env.GetShadowBranchName()
	finalCheckpointPath := paths.EntireMetadataDir + "/" + sess.ID + "/tasks/" + taskToolUseID + "/" + paths.CheckpointFileName
	if !env.FileExistsInBranch(shadowBranch, finalCheckpointPath) {
		t.Fatalf("final task checkpoint missing after subagent-stop: %s", finalCheckpointPath)
	}
	transcriptWantPath := paths.EntireMetadataDir + "/" + sess.ID + "/tasks/" + taskToolUseID + "/" + paths.AgentTranscriptFileName(subagentID)
	if !env.FileExistsInBranch(shadowBranch, transcriptWantPath) {
		t.Fatalf("subagent transcript not re-stored by the Final capture at %s", transcriptWantPath)
	}
}

// TestSubagentCheckpoints_IdleCommitNoMarkers_NoTrailer is the companion guard:
// an ordinary commit made while a session is IDLE with no in-flight background
// task must stay exactly as before this PR — unlinked. idleWithLiveMarker's
// eligibility widening must not become "any IDLE session with a transcript
// links every later commit," which would turn routine human commits made
// between agent turns into false session linkage.
func TestSubagentCheckpoints_IdleCommitNoMarkers_NoTrailer(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	sess.CreateTranscript("a normal turn with no background work", nil)

	if err := env.SimulateUserPromptSubmit(sess.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}

	state, err := env.GetSessionState(sess.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || state.Phase != session.PhaseIdle {
		t.Fatalf("expected session to be IDLE after turn-end, got %+v", state)
	}
	if len(state.InFlightTasks) != 0 {
		t.Fatalf("expected no in-flight markers, got %+v", state.InFlightTasks)
	}

	// An unrelated file is committed while the session sits idle, no TTY
	// (agent-mode commit) — same shape as a background subagent commit,
	// minus the marker.
	env.WriteFile("docs/unrelated.md", "# Unrelated\n")
	env.GitCommitWithShadowHooksAsAgent("Add unrelated doc", "docs/unrelated.md")

	headHash := env.GetHeadHash()
	if checkpointID := env.GetCheckpointIDFromCommitMessage(headHash); checkpointID != "" {
		t.Errorf("ordinary idle commit with no in-flight markers should not carry an Entire-Checkpoint trailer, got %s", checkpointID)
	}
}
