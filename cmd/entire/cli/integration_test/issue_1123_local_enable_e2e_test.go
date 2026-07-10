//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIssue1123_LocalOnlyEnable_HooksSaveCheckpoint is a full-flow reproduction
// of #1123: `entire enable --local` writes only .entire/settings.local.json, and
// the hooks (gated on settings.IsSetUpAndEnabled) silently no-op'd because that
// check only looked at settings.json — so a commit produced no checkpoint.
//
// This drives the real hook binary end-to-end: a session, a user-prompt-submit,
// a file change, a stop, and a commit — then asserts the commit actually carries
// an Entire-Checkpoint trailer (i.e. the hooks ran and a checkpoint was saved).
func TestIssue1123_LocalOnlyEnable_HooksSaveCheckpoint(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()
	env.WriteFile("README.md", "# Test")
	env.GitAdd("README.md")
	env.GitCommit("Initial commit")
	env.GitCheckoutNewBranch("feature/local-only")

	// Simulate `entire enable --local`: only settings.local.json exists.
	entireDir := filepath.Join(env.RepoDir, ".entire")
	if err := os.MkdirAll(filepath.Join(entireDir, "tmp"), 0o755); err != nil {
		t.Fatalf("mkdir .entire/tmp: %v", err)
	}
	localSettings := `{"enabled":true,"local_dev":true,"strategy_options":{"filtered_fetches":true}}`
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(localSettings), 0o644); err != nil {
		t.Fatalf("write settings.local.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(entireDir, "settings.json")); err == nil {
		t.Fatal("precondition: settings.json must not exist for the enable --local scenario")
	}

	session := env.NewSession()
	if err := env.SimulateUserPromptSubmitWithPrompt(session.ID, "Create a hello file"); err != nil {
		t.Fatalf("user-prompt-submit: %v", err)
	}
	env.WriteFile("hello.txt", "hello")
	session.CreateTranscript("Create a hello file", []FileChange{{Path: "hello.txt", Content: "hello"}})
	if err := env.SimulateStop(session.ID, session.TranscriptPath); err != nil {
		t.Fatalf("stop: %v", err)
	}
	env.GitCommitWithShadowHooksAsAgent("add hello", "hello.txt")

	cpID := env.GetCheckpointIDFromCommitMessage(env.GetHeadHash())
	if cpID == "" {
		t.Fatal("commit has no Entire-Checkpoint trailer — hooks silently no-op'd with only settings.local.json (#1123)")
	}
}
