//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// TestSubagentCheckpoints_StoresSubagentTranscript_CurrentLayout verifies that the
// task checkpoint captures the subagent's own transcript when it is stored the way
// current Claude Code versions store it: <transcriptDir>/<sessionID>/subagents/.
//
// Resolving only the legacy sibling layout (<transcriptDir>/agent-<id>.jsonl) meant
// the path never existed for a modern session, so the subagent transcript was never
// committed and file extraction fell back to the main transcript — where a
// subagent's Write/Edit calls do not appear.
func TestSubagentCheckpoints_StoresSubagentTranscript_CurrentLayout(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("delegate red.md to a subagent", nil)

	const (
		taskToolUseID = "toolu_01LayoutABC123"
		subagentID    = "a0123456789abcdef"
	)
	writeSubagentTranscript(t, session, subagentID, "docs/red.md")

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}

	// The subagent edits a file; only its own transcript records the Write.
	env.WriteFile("docs/red.md", "Red is a warm colour.\n")

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	wantPath := ".entire/metadata/" + session.ID + "/tasks/" + taskToolUseID + "/agent-" + subagentID + ".jsonl"
	content, ok := readShadowFile(t, env, wantPath)
	if !ok {
		t.Fatalf("subagent transcript not stored in shadow branch at %s", wantPath)
	}
	if !strings.Contains(content, "docs/red.md") {
		t.Errorf("stored subagent transcript does not reference the subagent's edit: %q", content)
	}
}

// TestSubagentCheckpoints_StoresSubagentTranscript_LegacyLayout keeps the older
// sibling layout working, so sessions recorded by earlier agent versions still get
// their subagent transcript captured.
func TestSubagentCheckpoints_StoresSubagentTranscript_LegacyLayout(t *testing.T) {
	t.Parallel()
	env := NewFeatureBranchEnv(t)
	session := env.NewSession()
	session.CreateTranscript("delegate blue.md to a subagent", nil)

	const (
		taskToolUseID = "toolu_01LegacyABC123"
		subagentID    = "afedcba9876543210"
	)
	// Legacy: a sibling of the main transcript, with no <sessionID>/subagents/ dir.
	legacyPath := filepath.Join(filepath.Dir(session.TranscriptPath), "agent-"+subagentID+".jsonl")
	writeTranscriptFile(t, legacyPath, "docs/blue.md")

	if err := env.SimulateUserPromptSubmit(session.ID); err != nil {
		t.Fatalf("SimulateUserPromptSubmit failed: %v", err)
	}
	if err := env.SimulatePreTask(session.ID, session.TranscriptPath, taskToolUseID); err != nil {
		t.Fatalf("SimulatePreTask failed: %v", err)
	}
	env.WriteFile("docs/blue.md", "Blue is a cool colour.\n")

	if err := env.SimulatePostTask(PostTaskInput{
		SessionID:      session.ID,
		TranscriptPath: session.TranscriptPath,
		ToolUseID:      taskToolUseID,
		AgentID:        subagentID,
	}); err != nil {
		t.Fatalf("SimulatePostTask failed: %v", err)
	}

	wantPath := ".entire/metadata/" + session.ID + "/tasks/" + taskToolUseID + "/agent-" + subagentID + ".jsonl"
	if _, ok := readShadowFile(t, env, wantPath); !ok {
		t.Fatalf("subagent transcript not stored in shadow branch at %s", wantPath)
	}
}

// writeSubagentTranscript writes a subagent transcript where current Claude Code
// versions put it: <transcriptDir>/<sessionID>/subagents/agent-<id>.jsonl, next to
// the agent-<id>.meta.json sidecar the agent also writes.
func writeSubagentTranscript(t *testing.T, session *Session, subagentID, editedPath string) {
	t.Helper()

	dir := filepath.Join(filepath.Dir(session.TranscriptPath), session.ID, "subagents")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("failed to create subagents dir: %v", err)
	}
	writeTranscriptFile(t, filepath.Join(dir, "agent-"+subagentID+".jsonl"), editedPath)

	meta := `{"agentType":"dev","description":"write ` + editedPath + `","spawnDepth":1}`
	metaPath := filepath.Join(dir, "agent-"+subagentID+".meta.json")
	if err := os.WriteFile(metaPath, []byte(meta), 0o600); err != nil {
		t.Fatalf("failed to write subagent meta: %v", err)
	}
}

// writeTranscriptFile writes a minimal subagent transcript whose only tool use is a
// Write of editedPath.
func writeTranscriptFile(t *testing.T, path, editedPath string) {
	t.Helper()

	builder := NewTranscriptBuilder()
	builder.AddUserMessage("create " + editedPath)
	toolID := builder.AddToolUse("mcp__acp__Write", editedPath, "content")
	builder.AddToolResult(toolID)
	builder.AddAssistantMessage("Done!")

	if err := builder.WriteToFile(path); err != nil {
		t.Fatalf("failed to write transcript %s: %v", path, err)
	}
}

// readShadowFile returns the contents of a path in the shadow branch tree.
func readShadowFile(t *testing.T, env *TestEnv, path string) (string, bool) {
	t.Helper()

	repo, err := git.PlainOpen(env.RepoDir)
	if err != nil {
		t.Fatalf("failed to open repo: %v", err)
	}

	shadowBranchName := env.GetShadowBranchName()
	shadowRef, err := repo.Reference(plumbing.NewBranchReferenceName(shadowBranchName), true)
	if err != nil {
		t.Fatalf("shadow branch %s not found: %v", shadowBranchName, err)
	}
	shadowCommit, err := repo.CommitObject(shadowRef.Hash())
	if err != nil {
		t.Fatalf("failed to get shadow commit: %v", err)
	}
	shadowTree, err := shadowCommit.Tree()
	if err != nil {
		t.Fatalf("failed to get shadow tree: %v", err)
	}

	var content string
	var found bool
	if err := shadowTree.Files().ForEach(func(f *object.File) error {
		if f.Name != path {
			return nil
		}
		c, readErr := f.Contents()
		if readErr != nil {
			return readErr
		}
		content, found = c, true
		return nil
	}); err != nil {
		t.Fatalf("failed to iterate shadow tree: %v", err)
	}
	return content, found
}
