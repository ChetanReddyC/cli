//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TestSubagentCheckpoints_FullFlow tests the complete subagent checkpoint flow:
// PreTask -> PostTodo (multiple times with file changes) -> PostTask
//
// This verifies:
// 1. Incremental checkpoints are created as commits during subagent execution
// 2. Only PostTodo calls with file changes create commits
// 3. PostTask creates the final task checkpoint commit
func TestSubagentCheckpoints_FullFlow(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Create a session
	session := env.NewSession()

	// Create transcript (needed by hooks)
	session.CreateTranscript("Implement feature X", []FileChange{
		{Path: "feature.go", Content: "package main"},
	})

	// Simulate user prompt submit first (captures pre-prompt state)
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Task tool use ID (simulates Claude's Task tool invocation)
	taskToolUseID := "toolu_01TaskABC123"

	// Step 1: PreTask - creates pre-task file
	err = env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID)
	if err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Verify pre-task file was created
	preTaskFile := filepath.Join(env.RepoDir, ".entire", "tmp", "pre-task-"+taskToolUseID+".json")
	if _, err := os.Stat(preTaskFile); os.IsNotExist(err) {
		t.Error("pre-task file should exist after SimulatePreTask")
	}

	// Step 2: PostTodo - simulate TodoWrite calls with file changes between them
	// Note: Only PostTodo calls that detect file changes will create incremental commits

	// First TodoWrite - no file changes, should be skipped
	err = env.SimulatePostTodo(PostTodoInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      "toolu_01TodoWrite001",
		Todos: []Todo{
			{Content: "Create feature file", Status: "in_progress", ActiveForm: "Creating feature file"},
			{Content: "Write tests", Status: "pending", ActiveForm: "Writing tests"},
		},
	})
	if err != nil {
		t.Fatalf("SimulatePostTodo failed for first todo: %v", err)
	}

	// Create a file change
	env.WriteFile("feature.go", "package main\n\nfunc Feature() {}\n")

	// Second TodoWrite - should create incremental checkpoint (has file changes)
	err = env.SimulatePostTodo(PostTodoInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      "toolu_01TodoWrite002",
		Todos: []Todo{
			{Content: "Create feature file", Status: "completed", ActiveForm: "Creating feature file"},
			{Content: "Write tests", Status: "in_progress", ActiveForm: "Writing tests"},
		},
	})
	if err != nil {
		t.Fatalf("SimulatePostTodo failed for second todo: %v", err)
	}

	// Create another file change
	env.WriteFile("feature_test.go", "package main\n\nimport \"testing\"\n\nfunc TestFeature(t *testing.T) {}\n")

	// Third TodoWrite - should create another incremental checkpoint
	err = env.SimulatePostTodo(PostTodoInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      "toolu_01TodoWrite003",
		Todos: []Todo{
			{Content: "Create feature file", Status: "completed", ActiveForm: "Creating feature file"},
			{Content: "Write tests", Status: "completed", ActiveForm: "Writing tests"},
		},
	})
	if err != nil {
		t.Fatalf("SimulatePostTodo failed for third todo: %v", err)
	}

	// Step 3: PostTask - creates final task checkpoint
	err = env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        "agent-123",
	})
	if err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	// Verify pre-task file is cleaned up
	if _, err := os.Stat(preTaskFile); !os.IsNotExist(err) {
		t.Error("Pre-task file should be removed after PostTask")
	}

	// Verify checkpoints are stored in final location (strategy-specific)
	verifyCheckpointStorage(t, env, session.ID, taskToolUseID)
}

// TestSubagentCheckpoints_NoFileChanges tests that PostTodo is skipped when no file changes
func TestSubagentCheckpoints_NoFileChanges(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Create a session
	session := env.NewSession()

	// Create transcript
	session.CreateTranscript("Quick task", []FileChange{})

	// Simulate user prompt submit
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create pre-task file to simulate subagent context
	taskToolUseID := "toolu_01TaskNoChanges"
	err = env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID)
	if err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Get git log before PostTodo
	beforeCommits := env.GetGitLog()

	// Call PostTodo WITHOUT making any file changes
	err = env.SimulatePostTodo(PostTodoInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      "toolu_01TodoWriteNoChange",
		Todos: []Todo{
			{Content: "Some task", Status: "pending", ActiveForm: "Doing task"},
		},
	})
	if err != nil {
		t.Fatalf("SimulatePostTodo should not fail: %v", err)
	}

	// Get git log after PostTodo
	afterCommits := env.GetGitLog()

	// Verify no new commits were created
	if len(afterCommits) != len(beforeCommits) {
		t.Errorf("Expected no new commits when no file changes, before=%d after=%d", len(beforeCommits), len(afterCommits))
	}
}

// TestSubagentCheckpoints_PostTaskNoFileChanges tests that PostTask is skipped when no file changes
// and the pre-task state is still cleaned up.
func TestSubagentCheckpoints_PostTaskNoFileChanges(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Create a session
	session := env.NewSession()

	// Create transcript (no file changes in transcript either)
	session.CreateTranscript("Quick task with no file changes", []FileChange{})

	// Simulate user prompt submit
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create pre-task file to simulate subagent context
	taskToolUseID := "toolu_01TaskNoFileChanges"
	err = env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID)
	if err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Verify pre-task file was created
	preTaskFile := filepath.Join(env.RepoDir, ".entire", "tmp", "pre-task-"+taskToolUseID+".json")
	if _, err := os.Stat(preTaskFile); os.IsNotExist(err) {
		t.Fatal("pre-task file should exist after SimulatePreTask")
	}

	// Get git log before PostTask
	beforeCommits := env.GetGitLog()

	// Call PostTask WITHOUT making any file changes
	err = env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        "agent-no-changes",
	})
	if err != nil {
		t.Fatalf("SimulatePostTask should not fail: %v", err)
	}

	// Get git log after PostTask
	afterCommits := env.GetGitLog()

	// Verify no new commits were created on the main branch
	if len(afterCommits) != len(beforeCommits) {
		t.Errorf("Expected no new commits when no file changes, before=%d after=%d", len(beforeCommits), len(afterCommits))
	}

	// Verify pre-task file is cleaned up even though no checkpoint was created
	if _, err := os.Stat(preTaskFile); !os.IsNotExist(err) {
		t.Error("Pre-task file should be removed after PostTask even with no file changes")
	}
}

// TestSubagentCheckpoints_NoPreTaskFile tests that PostTodo is a no-op
// when there's no active pre-task file (main agent context).
func TestSubagentCheckpoints_NoPreTaskFile(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	// Create a session
	session := env.NewSession()

	// Create transcript
	session.CreateTranscript("Quick task", []FileChange{})

	// Simulate user prompt submit
	err := env.SimulateUserPromptSubmit(session.ID)
	if err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}

	// Create a file change so that PostTodo would trigger if in subagent context
	env.WriteFile("test.txt", "content")

	// Get git log before PostTodo
	beforeCommits := env.GetGitLog()

	// Call PostTodo WITHOUT calling PreTask first
	// This simulates a TodoWrite from the main agent (not a subagent)
	err = env.SimulatePostTodo(PostTodoInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      "toolu_01MainAgentTodo",
		Todos: []Todo{
			{Content: "Some task", Status: "pending", ActiveForm: "Doing task"},
		},
	})
	if err != nil {
		t.Fatalf("SimulatePostTodo should not fail: %v", err)
	}

	// Get git log after PostTodo
	afterCommits := env.GetGitLog()

	// Verify no new commits were created (not in subagent context)
	if len(afterCommits) != len(beforeCommits) {
		t.Errorf("Expected no new commits when not in subagent context, before=%d after=%d", len(beforeCommits), len(afterCommits))
	}
}

// verifyCheckpointStorage verifies that checkpoints are stored in the correct
// location based on the strategy type.
// Note: Incremental checkpoints are stored in separate commits during task execution,
// while the final checkpoint.json is created at PostTask time.
func verifyCheckpointStorage(t *testing.T, env *TestEnv, sessionID, taskToolUseID string) {
	t.Helper()

	// Manual-commit stores checkpoints in git tree on shadow branch (entire/<head-hash>)
	// We need to verify that checkpoint data exists in the shadow branch tree
	verifyShadowCheckpointStorage(t, env, sessionID, taskToolUseID)
}

// verifyShadowCheckpointStorage verifies that checkpoints are stored in the shadow branch git tree.
func verifyShadowCheckpointStorage(t *testing.T, env *TestEnv, sessionID, taskToolUseID string) {
	t.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	// Get shadow branch name using worktree-specific naming
	shadowBranchName := env.GetShadowBranchName()

	// Get shadow branch reference
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranchName), true)
	if err != nil {
		t.Fatalf("shadow branch %s not found: %v", shadowBranchName, err)
	}

	// Get the commit and tree from shadow branch
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	if err != nil {
		t.Fatalf("failed to get shadow commit: %v", err)
	}

	shadowTree, err := shadowCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get shadow tree: %v", err)
	}

	// Look for task metadata in the tree
	// Path format: .entire/metadata/<session-id>/tasks/<task-id>/
	taskMetadataPrefix := ".entire/metadata/" + sessionID + "/tasks/" + taskToolUseID + "/"
	checkpointsPrefix := taskMetadataPrefix + "checkpoints/"

	foundCheckpoint := false
	foundCheckpointFiles := 0

	err = shadowTree.Files().ForEach(func(f *object.File) error {
		// Check for checkpoint file (final checkpoint)
		if f.Name == taskMetadataPrefix+paths.CheckpointFileName {
			foundCheckpoint = true
			// Verify content is valid JSON
			content, readErr := f.Contents()
			if readErr != nil {
				t.Errorf("failed to read %s: %v", paths.CheckpointFileName, readErr)
				return nil
			}
			var cp strategy.TaskCheckpoint
			if jsonErr := json.Unmarshal([]byte(content), &cp); jsonErr != nil {
				t.Errorf("%s is invalid JSON: %v", paths.CheckpointFileName, jsonErr)
			}
		}

		// Check for incremental checkpoints in checkpoints/ directory
		if strings.HasPrefix(f.Name, checkpointsPrefix) && strings.HasSuffix(f.Name, ".json") {
			foundCheckpointFiles++
			// Verify content is valid checkpoint JSON
			content, readErr := f.Contents()
			if readErr != nil {
				t.Errorf("failed to read checkpoint file %s: %v", f.Name, readErr)
				return nil
			}
			var cp strategy.SubagentCheckpoint
			if jsonErr := json.Unmarshal([]byte(content), &cp); jsonErr != nil {
				t.Errorf("checkpoint file %s is invalid JSON: %v", f.Name, jsonErr)
			}
			// Verify required fields
			if cp.Type == "" {
				t.Errorf("checkpoint file %s missing type field", f.Name)
			}
			if cp.ToolUseID == "" {
				t.Errorf("checkpoint file %s missing tool_use_id field", f.Name)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to iterate shadow tree: %v", err)
	}

	if !foundCheckpoint {
		t.Errorf("%s not found in shadow branch tree at %s", paths.CheckpointFileName, taskMetadataPrefix+paths.CheckpointFileName)
	}

	if foundCheckpointFiles == 0 {
		t.Logf("Note: no incremental checkpoint files found in %s - they may be in earlier commits", checkpointsPrefix)
	} else {
		t.Logf("Found %d incremental checkpoint files in shadow branch tree", foundCheckpointFiles)
	}
}

// hasInFlightTask reports whether state has an in-flight marker for toolUseID.
func hasInFlightTask(state *strategy.SessionState, toolUseID string) bool {
	for _, task := range state.InFlightTasks {
		if task.ToolUseID == toolUseID {
			return true
		}
	}
	return false
}

// requireSessionState loads sessionID's persisted state and fails the test
// immediately if it can't be read or doesn't exist. Centralizing the nil
// guard here keeps callers that need a non-nil state (hasInFlightTask and
// State.FindInFlightTask both dereference unconditionally) from having to
// duplicate — and risk dropping — the check at each read site.
func requireSessionState(t *testing.T, env *TestEnv, sessionID string) *strategy.SessionState {
	t.Helper()
	state, err := env.GetSessionState(sessionID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil {
		t.Fatalf("expected session state to exist for session %s", sessionID)
	}
	return state
}

// TestSubagentCheckpoints_BackgroundLaunch_DefersToSubagentStop covers the
// background-subagent bug this PR fixes: Claude Code background subagents
// (run_in_background: true) return a launch stub immediately, so post-task
// (PostToolUse) used to fire seconds after launch — before the subagent had
// done any real work — and save (or skip) a task step from that stub alone.
// The real completion signal, SubagentStop, fired no hook entire listened to,
// so everything the subagent actually did was invisible to entire.
//
// This verifies the fix end to end, using a realistic Claude Code subagent
// transcript (session.CreateSubagentTranscript — the same builder
// TestSubagentCheckpoints_StoresSubagentTranscript uses) so the real
// transcript analyzer, not a stub, is what extracts the modified file:
//  1. post-task with run_in_background: true records an in-flight marker and
//     writes NO task checkpoint — capture is deferred, not lost.
//  2. subagent-stop (the authoritative final capture) writes the task
//     checkpoint with the subagent's real modified file and its own
//     transcript, and clears the marker.
func TestSubagentCheckpoints_BackgroundLaunch_DefersToSubagentStop(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("delegate a background task", nil)

	const (
		taskToolUseID = "toolu_01BackgroundABC123"
		subagentID    = "a1111222233334444"
		editedFile    = "docs/background.md"
	)

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// Launch stub: PostToolUse fires immediately with run_in_background: true
	// and the launch-assigned agentId, before the subagent has done any work.
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:       session.ID,
		TranscriptPath:  session.TranscriptPath,
		ToolUseID:       taskToolUseID,
		AgentID:         subagentID,
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
	}

	// Marker recorded; no task checkpoint written from the stub.
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || !hasInFlightTask(state, taskToolUseID) {
		t.Fatalf("expected in-flight marker for %s after background launch stub, state=%+v", taskToolUseID, state)
	}

	checkpointPath := paths.EntireMetadataDir + "/" + session.ID + "/tasks/" + taskToolUseID + "/" + paths.CheckpointFileName
	if env.FileExistsInBranch(env.GetShadowBranchName(), checkpointPath) {
		t.Fatalf("task checkpoint should not exist after the background launch stub (deferred to subagent-stop): %s", checkpointPath)
	}

	// The subagent does its actual work: a realistic transcript (parsed by the
	// real Claude Code transcript analyzer, not a stub) plus the resulting
	// uncommitted file.
	subagentTranscriptPath := session.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: editedFile, Content: "# Background\n"},
	})
	env.WriteFile(editedFile, "# Background\n\nWritten by a background subagent.\n")

	// The real completion signal.
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:           session.ID,
		TranscriptPath:      session.TranscriptPath,
		AgentID:             subagentID,
		AgentTranscriptPath: subagentTranscriptPath,
		ToolUseID:           taskToolUseID,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop failed: %v", err)
	}

	// Marker cleared.
	state, err = env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state != nil && hasInFlightTask(state, taskToolUseID) {
		t.Errorf("in-flight marker for %s should be cleared after subagent-stop", taskToolUseID)
	}

	// Task checkpoint now exists.
	shadowBranch := env.GetShadowBranchName()
	if !env.FileExistsInBranch(shadowBranch, checkpointPath) {
		t.Fatalf("task checkpoint missing after subagent-stop: %s", checkpointPath)
	}

	// The subagent's own transcript is stored and references the edited file.
	// This pins what got captured, not how: the capture path merges the real
	// transcript analyzer's output with git-status detection, so a second
	// untracked file would also show up here via git status by design (unit
	// tests cover the analyzer-extraction split in isolation).
	transcriptWantPath := paths.EntireMetadataDir + "/" + session.ID + "/tasks/" + taskToolUseID + "/" + paths.AgentTranscriptFileName(subagentID)
	content, ok := env.ReadFileFromBranch(shadowBranch, transcriptWantPath)
	if !ok {
		t.Fatalf("subagent transcript not stored at %s", transcriptWantPath)
	}
	if !strings.Contains(content, editedFile) {
		t.Errorf("stored subagent transcript does not reference the modified file %q: %q", editedFile, content)
	}

	// The modified file's real content was captured into the shadow tree at
	// its real repo path — proof the checkpoint carries the subagent's actual
	// work, not just an empty stub.
	if !env.FileExistsInBranch(shadowBranch, editedFile) {
		t.Errorf("modified file %s not captured into shadow branch tree", editedFile)
	}
}

// TestSubagentCheckpoints_TurnEndBackstop_ThenSubagentStop covers the
// turn-end incremental backstop between a background launch stub and the
// eventual subagent-stop: before this PR, a background subagent's in-flight
// work had zero checkpoint presence until (if ever) SubagentStop arrived.
// Now turn-end (Stop) snapshots the subagent's code changes incrementally —
// leaving the in-flight marker in place, since the task is still running —
// and subagent-stop remains the authoritative final capture that clears it.
func TestSubagentCheckpoints_TurnEndBackstop_ThenSubagentStop(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("delegate a background task", nil)

	const (
		taskToolUseID = "toolu_01TurnEndBackstop"
		subagentID    = "b2222333344445555"
		editedFile    = "docs/turnend.md"
	)

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:       session.ID,
		TranscriptPath:  session.TranscriptPath,
		ToolUseID:       taskToolUseID,
		AgentID:         subagentID,
		RunInBackground: true,
	}); err != nil {
		t.Fatalf("SimulatePostTask (background stub) failed: %v", err)
	}

	// The subagent has made progress by the time the parent's turn ends, but
	// SubagentStop hasn't arrived yet.
	subagentTranscriptPath := session.CreateSubagentTranscript(subagentID, []FileChange{
		{Path: editedFile, Content: "# In progress\n"},
	})
	env.WriteFile(editedFile, "# In progress\n\nStill running.\n")

	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("SimulateStop (turn-end) failed: %v", err)
	}

	// Incremental checkpoint exists from the turn-end backstop.
	shadowBranch := env.GetShadowBranchName()
	incrementalPath := paths.EntireMetadataDir + "/" + session.ID + "/tasks/" + taskToolUseID +
		"/checkpoints/001-" + taskToolUseID + ".json"
	if !env.FileExistsInBranch(shadowBranch, incrementalPath) {
		t.Fatalf("expected incremental checkpoint from turn-end backstop at %s", incrementalPath)
	}

	// The marker survives: the task is still running, and subagent-stop
	// remains the authoritative final capture.
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state == nil || !hasInFlightTask(state, taskToolUseID) {
		t.Fatalf("expected in-flight marker for %s to survive turn-end, state=%+v", taskToolUseID, state)
	}

	// The final (non-incremental) task checkpoint does not exist yet.
	finalCheckpointPath := paths.EntireMetadataDir + "/" + session.ID + "/tasks/" + taskToolUseID + "/" + paths.CheckpointFileName
	if env.FileExistsInBranch(shadowBranch, finalCheckpointPath) {
		t.Fatalf("final task checkpoint should not exist before subagent-stop: %s", finalCheckpointPath)
	}

	// subagent-stop arrives: the authoritative final capture.
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:           session.ID,
		TranscriptPath:      session.TranscriptPath,
		AgentID:             subagentID,
		AgentTranscriptPath: subagentTranscriptPath,
		ToolUseID:           taskToolUseID,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop failed: %v", err)
	}

	// Final capture landed; marker cleared.
	shadowBranch = env.GetShadowBranchName()
	if !env.FileExistsInBranch(shadowBranch, finalCheckpointPath) {
		t.Fatalf("final task checkpoint missing after subagent-stop: %s", finalCheckpointPath)
	}

	state, err = env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state != nil && hasInFlightTask(state, taskToolUseID) {
		t.Errorf("in-flight marker for %s should be cleared after subagent-stop", taskToolUseID)
	}
}

// TestSubagentCheckpoints_ForegroundDoubleFire_CapturesOnce: a foreground task
// (no run_in_background) is captured immediately at post-task time and never
// records an in-flight marker. Claude Code fires SubagentStop for every
// completed Task, foreground and background alike, so entire also sees a
// SubagentStop for this same tool_use_id after the foreground capture already
// ran. Without the marker-claim guard, that second event would re-run the
// capture and produce a duplicate task checkpoint / commit. This verifies the
// regression is closed: exactly one task checkpoint is written, no in-flight
// marker is ever created for a foreground task, and the SubagentStop
// double-fire produces no additional commit.
func TestSubagentCheckpoints_ForegroundDoubleFire_CapturesOnce(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("run a foreground task", nil)

	const (
		taskToolUseID = "toolu_01ForegroundDoubleFire"
		subagentID    = "c3333444455556666"
		editedFile    = "docs/foreground.md"
	)

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	env.WriteFile(editedFile, "# Foreground\n\nWritten by a foreground subagent.\n")

	// Foreground completion: PostToolUse fires with no run_in_background, so
	// this is captured immediately — the existing, unchanged behavior.
	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	// Foreground tasks never record an in-flight marker.
	state, err := env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state != nil && hasInFlightTask(state, taskToolUseID) {
		t.Fatalf("foreground task should never record an in-flight marker, state=%+v", state)
	}

	checkpointPath := paths.EntireMetadataDir + "/" + session.ID + "/tasks/" + taskToolUseID + "/" + paths.CheckpointFileName
	shadowBranch := env.GetShadowBranchName()
	if !env.FileExistsInBranch(shadowBranch, checkpointPath) {
		t.Fatalf("task checkpoint missing after foreground post-task: %s", checkpointPath)
	}
	commitsAfterPostTask := env.GetGitLog()

	// The double-fire: SubagentStop for the same tool_use_id, with no marker
	// left to claim. Must be a no-op, not a second capture.
	if err := env.SimulateSubagentStop(SubagentStopInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		AgentID:        subagentID,
		ToolUseID:      taskToolUseID,
	}); err != nil {
		t.Fatalf("SimulateSubagentStop failed: %v", err)
	}

	commitsAfterSubagentStop := env.GetGitLog()
	if len(commitsAfterSubagentStop) != len(commitsAfterPostTask) {
		t.Errorf("SubagentStop double-fire created a new commit: before=%d after=%d",
			len(commitsAfterPostTask), len(commitsAfterSubagentStop))
	}

	// Still exactly one task checkpoint, and still no marker was ever
	// created.
	shadowBranch = env.GetShadowBranchName()
	if !env.FileExistsInBranch(shadowBranch, checkpointPath) {
		t.Fatalf("task checkpoint missing after subagent-stop double-fire: %s", checkpointPath)
	}
	state, err = env.GetSessionState(session.ID)
	if err != nil {
		t.Fatalf("GetSessionState failed: %v", err)
	}
	if state != nil && hasInFlightTask(state, taskToolUseID) {
		t.Errorf("in-flight marker for %s should not exist after a foreground double-fire", taskToolUseID)
	}
}
