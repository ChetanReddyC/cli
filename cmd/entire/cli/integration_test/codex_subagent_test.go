//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/stretchr/testify/require"
)

// TestCodexSubagent_StoresDeclaredSubagentTranscript drives Codex's real subagent
// hooks end to end: subagent-start → the subagent edits a file → subagent-stop, and
// asserts the task checkpoint stores the subagent's own transcript.
//
// The point is the *declared* path. Codex sends agent_transcript_path in the
// payload, and its rollouts live nowhere near Claude Code's
// <dir>/<sessionID>/subagents/ layout — so if the framework guessed instead of
// honouring the declaration, this transcript would silently not be stored (the
// failure mode that #1935 fixed for Claude Code and that Cursor still had).
func TestCodexSubagent_StoresDeclaredSubagentTranscript(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)

	const (
		sessionID  = "test-codex-subagent"
		agentID    = "child-thread-9"
		agentType  = "reviewer"
		editedFile = "docs/red.md"
	)

	// Session state with AgentType=Codex, as codex_post_tool_use_test.go does: the
	// dispatcher resolves the agent from the hook command, but the strategy reads
	// the agent type from state.
	statePath := filepath.Join(env.RepoDir, ".git", "entire-sessions", sessionID+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
	stateBytes, err := json.Marshal(map[string]any{
		"session_id":  sessionID,
		"agent_type":  "Codex",
		"base_commit": env.GetHeadHash(),
		"started_at":  time.Now().Format(time.RFC3339Nano),
		"step_count":  0,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, stateBytes, 0o600))

	// The parent rollout Codex reports as transcript_path.
	parentRollout := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollouts", "rollout-"+sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(parentRollout), 0o750))
	require.NoError(t, os.WriteFile(parentRollout, []byte(`{"type":"message","content":"delegate"}`+"\n"), 0o600))

	// Codex rollouts are flat files in its own directory — deliberately not the
	// nested layout the Claude Code fallback would probe.
	subagentRollout := filepath.Join(filepath.Dir(parentRollout), "rollout-"+agentID+".jsonl")
	require.NoError(t, os.WriteFile(subagentRollout,
		[]byte(`{"type":"message","content":"wrote `+editedFile+`"}`+"\n"), 0o600))

	runner := NewCodexHookRunner(env.RepoDir, t)
	if err := runner.SimulateCodexSubagentStart(sessionID, agentID, agentType, parentRollout); err != nil {
		t.Fatalf("SimulateCodexSubagentStart failed: %v", err)
	}

	env.WriteFile(editedFile, "Red is a warm colour.\n")

	if err := runner.SimulateCodexSubagentStop(sessionID, agentID, agentType, parentRollout, subagentRollout); err != nil {
		t.Fatalf("SimulateCodexSubagentStop failed: %v", err)
	}

	// Codex has no tool_use_id, so agent_id is the correlation key and therefore the
	// task directory name.
	wantPath := paths.EntireMetadataDir + "/" + sessionID +
		"/tasks/" + agentID + "/" + paths.AgentTranscriptFileName(agentID)
	content, ok := env.ReadFileFromBranch(env.GetShadowBranchName(), wantPath)
	if !ok {
		t.Fatalf("subagent transcript not stored at %s — the declared agent_transcript_path was not honoured", wantPath)
	}
	if !strings.Contains(content, editedFile) {
		t.Errorf("stored transcript is not the subagent's rollout: %q", content)
	}
}
