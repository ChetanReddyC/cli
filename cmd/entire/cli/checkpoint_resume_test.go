package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/spf13/cobra"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

func newCheckpointResumeTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newCheckpointResumeCmd()
	out := &bytes.Buffer{}
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetErr(out)
	return cmd, out
}

func TestCheckpointResume_RejectsPositionalWithTargetFlags(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	setupResumeTestRepo(t, tmpDir, false)

	for _, flag := range []string{"--checkpoint=abc123def456", "--commit=HEAD", "--branch=feature"} {
		cmd, _ := newCheckpointResumeTestCmd(t)
		cmd.SetArgs([]string{"sometarget", flag})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "cannot combine") {
			t.Errorf("Execute() with positional + %s: err = %v, want 'cannot combine'", flag, err)
		}
	}
}

// A ULID-shaped target that matches a committed checkpoint must resolve as a
// checkpoint even when a branch of the same name exists (checkpoint wins).
// The checkpoint's commit is on no branch, so the restore-only fallback runs
// and HEAD must not move.
func TestCheckpointResumeAuto_ChecksCheckpointBeforeBranch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	claudeDir := filepath.Join(tmpDir, "claude-projects")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)

	repo, _, head := setupResumeTestRepo(t, tmpDir, false)
	cpID := id.MustCheckpointID("01HZXW5J8KQ2M3N4P5Q6R7S8T9")
	writeCommittedResumeCheckpoint(t, repo, cpID, "session-cp-first", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	branchRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(cpID.String()), head)
	if err := repo.Storer.SetReference(branchRef); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{cpID.String()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "not on any local branch") {
		t.Errorf("output should mention restore-only fallback, got: %s", out.String())
	}
	branch, err := GetCurrentBranch(context.Background())
	if err != nil || branch != "master" {
		t.Errorf("HEAD moved: branch = %q err = %v, want master", branch, err)
	}
}

// A target that is a branch name (even hex-shaped) with no matching checkpoint
// must delegate to the branch flow, i.e. check the branch out.
func TestCheckpointResumeAuto_BranchBeforeCommit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	claudeDir := filepath.Join(tmpDir, "claude-projects")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)

	repo, w, _ := setupResumeTestRepo(t, tmpDir, false)
	// Ignore .entire/ (as `entire enable` would) so the RunE's logging.Init
	// creating .entire/logs/ doesn't register as an uncommitted change and
	// trip switchToBranchForResume's dirty-worktree check.
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(".entire/\n"), 0o600); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	if _, err := w.Add(".gitignore"); err != nil {
		t.Fatalf("add .gitignore: %v", err)
	}
	gitignoreCommit, err := w.Commit("add gitignore", &git.CommitOptions{
		Author: &object.Signature{Name: "Test User", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("commit .gitignore: %v", err)
	}
	branchRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("abcdef"), gitignoreCommit)
	if err := repo.Storer.SetReference(branchRef); err != nil {
		t.Fatalf("create branch: %v", err)
	}

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"abcdef"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	branch, err := GetCurrentBranch(context.Background())
	if err != nil || branch != "abcdef" {
		t.Errorf("current branch = %q err = %v, want abcdef", branch, err)
	}
}

// --commit on a trailer-carrying commit resumes that commit's checkpoint. The
// commit is on master (indexed by buildCheckpointBranchIndex) and master is
// already checked out, so the flow ends in a restored session.
func TestCheckpointResumeCommit_ResolvesTrailer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	claudeDir := filepath.Join(tmpDir, "claude-projects")
	t.Setenv("ENTIRE_TEST_CLAUDE_PROJECT_DIR", claudeDir)

	repo, w, _ := setupResumeTestRepo(t, tmpDir, false)
	cpID := id.MustCheckpointID("abc123def456")
	writeCommittedResumeCheckpoint(t, repo, cpID, "session-commit", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	commitHash, err := w.Commit("work\n\nEntire-Checkpoint: "+cpID.String(), &git.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "Test User", Email: "test@example.com"},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"--commit", commitHash.String()})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "session-commit") {
		t.Errorf("output should mention restored session ID, got: %s", out.String())
	}
}

func TestCheckpointResumeCommit_NoTrailer(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	_, _, head := setupResumeTestRepo(t, tmpDir, false)

	cmd, out := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"--commit", head.String()})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil, want error for commit without trailer")
	}
	if !strings.Contains(out.String(), "No associated Entire checkpoint") {
		t.Errorf("output should explain missing trailer, got: %s", out.String())
	}
}

func TestCheckpointResumeAuto_NothingMatched(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	setupResumeTestRepo(t, tmpDir, false)

	cmd, _ := newCheckpointResumeTestCmd(t)
	cmd.SetArgs([]string{"no/such-target"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "nothing matched") {
		t.Errorf("Execute() err = %v, want 'nothing matched'", err)
	}
}
