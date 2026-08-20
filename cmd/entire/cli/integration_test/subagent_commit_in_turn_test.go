//go:build integration

package integration

import (
	"strings"
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

	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	rec := state.FindTaskRecord(taskToolUseID)
	if rec == nil || rec.CompletedAt.IsZero() || !containsFile(rec.Files, editedFile) {
		t.Errorf("expected a completed task record carrying uncommitted subagent work, got %+v", rec)
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
// This drives the REAL prepare-commit-msg and post-commit git hooks end to end
// (GitCommitWithShadowHooksAsAgent), so it is the arbiter of whether the
// trailer tryAgentCommitFastPath adds (idleWithTaskContent eligibility,
// manual_commit_hooks.go) is actually backed by content: the commit's own
// condensation materializes the live task record's transcript-so-far into the
// permanent checkpoint's tasks/ subtree (#2058's durable-record model), so the
// checkpoint-content assertion below proves the trailer resolves to something
// real rather than dangling — reachable here only because idleWithTaskContent
// bypasses shouldCondenseWithOverlapCheck's overlap requirement for this IDLE,
// record-bearing session (its FilesTouched carries no evidence tying it to
// editedFile). No seed task is needed: findSessionsForWorktree's orphan sweep
// spares record-bearing sessions (manual_commit_session.go), so even a
// first-turn background commit links.
func TestSubagentCheckpoints_CommitWhileIdleWithLiveMarker_LinksAndCondensesContent(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	sess := env.NewSession()
	sess.CreateTranscript("delegate a background task", nil)

	const (
		taskToolUseID = "toolu_01IdleMarkerCommit"
		subagentID    = "d4444555566667777"
		editedFile    = "docs/idlemarker.md"
	)

	// Real Claude Code always sends a transcript_path on UserPromptSubmit; it
	// is what populates the persisted SessionState.TranscriptPath condensation
	// later stores as the parent transcript.
	if err := env.SimulateUserPromptSubmitWithTranscriptPath(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	if err := env.SimulatePreTask(sess.ID, sess.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Background launch: record created while the parent is still ACTIVE
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
	// running. The record survives, still live. This is the shape
	// idleWithTaskContent exists for: an IDLE session whose background task is
	// still genuinely in flight.
	if err := env.SimulateStop(sess.ID, sess.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop failed: %v", err)
	}
	state := requireSessionState(t, env, sess.ID)
	if state.Phase != session.PhaseIdle {
		t.Fatalf("expected session to be IDLE after turn-end, got %+v", state)
	}
	if !hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("expected live task record to survive turn-end, state=%+v", state)
	}

	// The subagent does its actual work while the parent sits idle between
	// turns: a realistic transcript (the real Claude Code transcript
	// analyzer, not a stub) plus the resulting file.
	const editedContent = "# Idle marker\n\nWritten by a background subagent while the parent is idle.\n"
	subagentTranscriptPath := sess.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: editedFile, Content: editedContent},
	})
	env.WriteFile(editedFile, editedContent)

	// The commit lands while the session is IDLE, through the real
	// prepare-commit-msg + post-commit hook chain, with no TTY (agent-mode
	// commit) — the exact shape of the incident.
	env.GitCommitWithShadowHooksAsAgent("Add idle-marker doc", editedFile)

	headHash := env.GetHeadHash()
	checkpointID := env.GetCheckpointIDFromCommitMessage(headHash)
	if checkpointID == "" {
		t.Fatalf("commit made while idle with a live task record should carry an Entire-Checkpoint trailer")
	}

	// THE content guarantee: the commit's condensation materialized the live
	// record's transcript-so-far into the permanent checkpoint's tasks/
	// subtree, so the trailer points at the subagent's real work.
	storedTranscript, ok := env.ReadFileFromBranch(paths.MetadataBranchName,
		CheckpointTaskFilePath(checkpointID, taskToolUseID, "agent-"+subagentID+".jsonl"))
	if !ok {
		t.Fatalf("subagent transcript not materialized under the checkpoint's tasks/ subtree")
	}
	if !strings.Contains(storedTranscript, editedFile) {
		t.Errorf("materialized subagent transcript does not reference %q: %q", editedFile, storedTranscript)
	}

	// The condensed permanent checkpoint resolves at all (rather than
	// PostCommit skipping this IDLE session for lack of overlap evidence) only
	// because idleWithTaskContent bypasses the overlap check, and it carries
	// editedFile in FilesTouched via CondenseSession's committed-files
	// fallback (filterFilesTouched in manual_commit_condensation.go).
	env.ValidateCheckpoint(CheckpointValidation{
		CheckpointID:              checkpointID,
		SessionID:                 sess.ID,
		FilesTouched:              []string{editedFile},
		ExpectedTranscriptContent: []string{"delegate a background task"},
	})

	// The live record survives condensation: the task is still running, and
	// SubagentStop (not this commit) remains the authoritative completion
	// signal; the next condensation re-materializes it.
	state = requireSessionState(t, env, sess.ID)
	if !hasLiveTaskRecord(state, taskToolUseID) {
		t.Fatalf("expected live task record for %s to survive condensation, state=%+v", taskToolUseID, state)
	}

	// The subagent now finishes: the real completion signal completes the
	// record exactly-once, leaving it present (for the next materialization)
	// but no longer live.
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:           sess.ID,
		TranscriptPath:      sess.TranscriptPath,
		AgentID:             subagentID,
		AgentTranscriptPath: subagentTranscriptPath,
		ToolUseID:           taskToolUseID,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop failed: %v", err)
	}

	state = requireSessionState(t, env, sess.ID)
	if hasLiveTaskRecord(state, taskToolUseID) {
		t.Errorf("task record for %s should be completed after subagent-stop", taskToolUseID)
	}
	if !hasTaskRecord(state, taskToolUseID) {
		t.Errorf("completed record for %s must persist until the next condensation materializes it", taskToolUseID)
	}
	// The already-committed file is not re-added as a tracked file: the
	// analyzer's ModifiedFiles included it, but filterToUncommittedFiles
	// strips anything already in HEAD before it ever reaches FilesTouched.
	for _, f := range state.FilesTouched {
		if f == editedFile {
			t.Errorf("committed file %s should not be re-added to FilesTouched by the completion capture", editedFile)
		}
	}
}

// TestSubagentCheckpoints_IdleCommitNoMarkers_NoTrailer is the companion guard:
// an ordinary commit made while a session is IDLE with no task records must
// stay exactly as before this PR — unlinked. idleWithTaskContent's
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
	if len(state.TaskRecords) != 0 {
		t.Fatalf("expected no task records, got %+v", state.TaskRecords)
	}

	// An unrelated file is committed while the session sits idle, no TTY
	// (agent-mode commit) — same shape as a background subagent commit,
	// minus the marker.
	env.WriteFile("docs/unrelated.md", "# Unrelated\n")
	env.GitCommitWithShadowHooksAsAgent("Add unrelated doc", "docs/unrelated.md")

	headHash := env.GetHeadHash()
	if checkpointID := env.GetCheckpointIDFromCommitMessage(headHash); checkpointID != "" {
		t.Errorf("ordinary idle commit with no task records should not carry an Entire-Checkpoint trailer, got %s", checkpointID)
	}
}
