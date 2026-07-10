package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

func TestDefaultTrailWorktreePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		branch      string
		trailNumber int
		want        string
	}{
		{"with number", "peter/feature.auth", 123, filepath.Join("/repo", ".entire", "worktrees", "trail-123-peter-feature.auth")},
		{"without number", "feature/other", 0, filepath.Join("/repo", ".entire", "worktrees", "feature-other")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultTrailWorktreePath("/repo", tt.branch, tt.trailNumber); got != tt.want {
				t.Fatalf("defaultTrailWorktreePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeTrailWorktreeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"feature/test", "feature-test"},
		{"Feat_1.2-x", "Feat_1.2-x"},
		{"weird name!", "weird-name"},
		{"---", trailWorktreeFallbackName},
		{"  spaced  ", "spaced"},
	}
	for _, tt := range tests {
		if got := sanitizeTrailWorktreeName(tt.in); got != tt.want {
			t.Fatalf("sanitizeTrailWorktreeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShellQuotePath(t *testing.T) {
	t.Parallel()

	got := shellQuotePath("/tmp/it's here")
	want := `'/tmp/it'\''s here'`
	if got != want {
		t.Fatalf("shellQuotePath() = %q, want %q", got, want)
	}
}

func TestAppendIgnoreRule(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "sub", "exclude")
	for range 2 {
		if err := appendIgnoreRule(path, ".entire/worktrees/"); err != nil {
			t.Fatalf("appendIgnoreRule: %v", err)
		}
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := strings.Count(string(content), ".entire/worktrees/"); got != 1 {
		t.Fatalf("rule count = %d, want 1; content: %q", got, string(content))
	}
	if !strings.HasSuffix(string(content), "\n") {
		t.Fatalf("content %q missing trailing newline", string(content))
	}
}

func TestAppendIgnoreRule_AddsNewlineBeforeRule(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "gitignore")
	if err := os.WriteFile(path, []byte("node_modules"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := appendIgnoreRule(path, ".entire/worktrees/"); err != nil {
		t.Fatalf("appendIgnoreRule: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := string(content), "node_modules\n.entire/worktrees/\n"; got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func TestEnsureTrailWorktreeIgnoreRule_NonTTYWritesExclude(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	t.Chdir(repoDir)

	var out bytes.Buffer
	if err := ensureTrailWorktreeIgnoreRule(context.Background(), &out, repoDir, false); err != nil {
		t.Fatalf("ensureTrailWorktreeIgnoreRule: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !strings.Contains(string(content), ".entire/worktrees/") {
		t.Fatalf("exclude = %q, want .entire/worktrees/ rule", string(content))
	}
	if !strings.Contains(out.String(), ".git/info/exclude") {
		t.Fatalf("output = %q, want notice mentioning .git/info/exclude", out.String())
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf(".gitignore stat = %v, want not exist", err)
	}
}

func TestEnsureTrailWorktreeIgnoreRule_AlreadyIgnoredIsSilentNoop(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", ".entire/worktrees/\n")
	t.Chdir(repoDir)

	var out bytes.Buffer
	if err := ensureTrailWorktreeIgnoreRule(context.Background(), &out, repoDir, false); err != nil {
		t.Fatalf("ensureTrailWorktreeIgnoreRule: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want silence", out.String())
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".git", "info", "exclude")); err == nil {
		content, readErr := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
		if readErr == nil && strings.Contains(string(content), ".entire/worktrees/") {
			t.Fatalf("exclude gained the rule despite .gitignore already covering it")
		}
	}
}
