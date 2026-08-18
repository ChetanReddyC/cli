package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
)

// TestWithHookSession_StampsMostRecentSession pins the one thing the hook path
// adds over the root prerun: lines logged under the returned context carry the
// session, which root cannot know without scanning session state on every
// command.
func TestWithHookSession_StampsMostRecentSession(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	testutil.InitRepo(t, tmpDir)

	entireDir := filepath.Join(tmpDir, paths.EntireDir)
	if err := os.MkdirAll(entireDir, 0o750); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}
	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled":true,"strategy":"manual-commit"}`), 0o600); err != nil {
		t.Fatalf("failed to create settings file: %v", err)
	}

	sessionID := "test-session-12345"
	writeTestSessionState(t, tmpDir, sessionID)

	// Stand in for the root prerun, which is now the only thing that opens the
	// log sink.
	l, err := logging.Init(context.Background())
	if err != nil {
		t.Fatalf("logging.Init() error = %v", err)
	}
	if l == nil {
		t.Fatal("logging.Init() returned no logger for a writable log directory")
	}
	t.Cleanup(logging.Close)

	ctx := withHookSession(logging.WithLogger(context.Background(), l))
	logging.Warn(ctx, "hook session stamped")
	logging.Close()

	content, err := os.ReadFile(filepath.Join(entireDir, "logs", "entire.log"))
	if err != nil {
		t.Fatalf("failed to read log file: %v", err)
	}
	if !strings.Contains(string(content), `"session_id":"`+sessionID+`"`) {
		t.Errorf("log line missing session_id=%s: %s", sessionID, content)
	}
}

// TestWithHookSession_GateSkipsRepo covers the gate that newAgentHooksCmd relies
// on entirely: a repo Entire never set up, and one where it is disabled, must
// come back with the context untouched — nothing scanned, no redactors loaded.
func TestWithHookSession_GateSkipsRepo(t *testing.T) {
	tests := []struct {
		name     string
		settings string
	}{
		{"never set up", ""},
		{"set up but disabled", `{"enabled":false,"strategy":"manual-commit"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Chdir(tmpDir)
			testutil.InitRepo(t, tmpDir)

			if tt.settings != "" {
				entireDir := filepath.Join(tmpDir, paths.EntireDir)
				if err := os.MkdirAll(entireDir, 0o750); err != nil {
					t.Fatalf("failed to create .entire directory: %v", err)
				}
				settingsFile := filepath.Join(entireDir, "settings.json")
				if err := os.WriteFile(settingsFile, []byte(tt.settings), 0o600); err != nil {
					t.Fatalf("failed to create settings file: %v", err)
				}
			}

			ctx := context.Background()
			if got := withHookSession(ctx); got != ctx {
				t.Error("withHookSession() returned a derived context; the gate must return the input unchanged")
			}
		})
	}
}

// TestHooksGitCmd_DiscoverExternalAgents_WhenEnabled verifies that when Entire is set up
// and enabled, PersistentPreRunE calls external.DiscoverAndRegister so that external
// agents are available during hook execution (e.g. post-commit condensation).
func TestHooksGitCmd_DiscoverExternalAgents_WhenEnabled(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	tmpDir := t.TempDir()

	// Initialize git repo first, then chdir so paths cache is correct
	gitInit := exec.CommandContext(context.Background(), "git", "init")
	gitInit.Dir = tmpDir
	if err := gitInit.Run(); err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	t.Chdir(tmpDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()

	// Reset global state before the test
	gitHooksDisabled = false

	// Create .entire/settings.json with enabled: true and external_agents: true
	entireDir := filepath.Join(tmpDir, paths.EntireDir)
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatalf("failed to create .entire directory: %v", err)
	}
	settingsFile := filepath.Join(entireDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte(`{"enabled":true,"external_agents":true}`), 0o644); err != nil {
		t.Fatalf("failed to write settings file: %v", err)
	}

	// Create a mock external agent binary in a temp PATH directory.
	// Use a unique name to avoid conflicts with agents registered by other tests.
	agentName := types.AgentName("hooktest-discovery-agent")
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "entire-agent-"+string(agentName))
	infoJSON := `{
  "protocol_version": 1,
  "name": "` + string(agentName) + `",
  "type": "Hook Test Agent",
  "description": "Agent for hook discovery test",
  "is_preview": false,
  "protected_dirs": [],
  "hook_names": [],
  "capabilities": {}
}`
	script := "#!/bin/sh\nif [ \"$1\" = \"info\" ]; then\n  echo '" + infoJSON + "'\nfi\n"
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock agent binary: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Execute the git hook command (post-commit) so PersistentPreRunE runs
	cmd := newHooksGitCmd()
	cmd.SetArgs([]string{"post-commit"})
	ctx := context.Background()
	cmd.SetContext(ctx)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("git hook command failed: %v", err)
	}

	// PersistentPreRunE should not have disabled hooks
	if gitHooksDisabled {
		t.Fatal("gitHooksDisabled should be false when Entire is enabled")
	}

	// The external agent should have been discovered and registered in the agent registry,
	// confirming that DiscoverAndRegister was called during PersistentPreRunE.
	if _, err := agent.Get(agentName); err != nil {
		t.Errorf("expected external agent %q to be registered after hook pre-run, got: %v", agentName, err)
	}
}

func TestHooksGitCmd_ExposesPostRewriteSubcommand(t *testing.T) {
	t.Parallel()

	cmd := newHooksGitCmd()
	found, _, err := cmd.Find([]string{"post-rewrite"})
	if err != nil {
		t.Fatalf("could not find post-rewrite subcommand: %v", err)
	}
	if found == nil {
		t.Fatal("expected post-rewrite subcommand, got nil")
		return
	}
	if found.Use != "post-rewrite <rewrite-type>" {
		t.Fatalf("post-rewrite Use = %q, want %q", found.Use, "post-rewrite <rewrite-type>")
	}
}

func TestHooksGitCommitMsgSkipsWhenPolicyUnsupported(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	gitHooksDisabled = false

	enableEntire(t, repoDir)

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	writeUnsupportedCheckpointPolicyForCLITest(t, repo)

	msgFile := filepath.Join(repoDir, "COMMIT_EDITMSG")
	message := []byte("Entire-Checkpoint: abc123def456\n")
	if err := os.WriteFile(msgFile, message, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newHooksGitCmd()
	cmd.SetArgs([]string{"commit-msg", msgFile})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("commit-msg should skip checkpoint work when policy is unsupported: %v", err)
	}

	got, err := os.ReadFile(msgFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(message) {
		t.Fatalf("commit message changed under unsupported policy:\ngot:\n%s\nwant:\n%s", got, message)
	}
}

func TestHooksGitCommitMsgSkipsWhenPolicyUnreadable(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "f.txt", "x")
	testutil.GitAdd(t, repoDir, "f.txt")
	testutil.GitCommit(t, repoDir, "init")
	t.Chdir(repoDir)
	paths.ClearWorktreeRootCache()
	session.ClearGitCommonDirCache()
	gitHooksDisabled = false

	enableEntire(t, repoDir)

	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	writeMalformedCheckpointPolicyForCLITest(t, repo)

	msgFile := filepath.Join(repoDir, "COMMIT_EDITMSG")
	message := []byte("Entire-Checkpoint: abc123def456\n")
	if err := os.WriteFile(msgFile, message, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newHooksGitCmd()
	cmd.SetArgs([]string{"commit-msg", msgFile})
	cmd.SetContext(context.Background())

	if err := cmd.Execute(); err != nil {
		t.Fatalf("commit-msg should skip checkpoint work when policy is unreadable: %v", err)
	}

	got, err := os.ReadFile(msgFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(message) {
		t.Fatalf("commit message changed under unreadable policy:\ngot:\n%s\nwant:\n%s", got, message)
	}
}

func TestGitHookPolicySkipsWhenRepoCannotOpen(t *testing.T) {
	t.Chdir(t.TempDir())
	paths.ClearWorktreeRootCache()

	g := &gitHookContext{
		hookName: "commit-msg",
		ctx:      context.Background(),
	}

	if !g.skipUnsupportedCheckpointPolicy() {
		t.Fatal("expected git hook to skip when repository cannot be opened")
	}
}
