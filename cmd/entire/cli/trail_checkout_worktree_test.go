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

const testEnvFile = ".env"

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

func TestMatchIncludePatterns(t *testing.T) {
	t.Parallel()

	files := []string{
		testEnvFile,
		"config/.env.local",
		".entire/worktrees/other/.env",
		"/abs/.env",
		"../escape/.env",
		"node_modules/pkg/x.js",
	}
	got := matchIncludePatterns([]string{testEnvFile, "*.local"}, files)
	want := []string{testEnvFile, filepath.Join("config", ".env.local")}
	if len(got) != len(want) {
		t.Fatalf("matchIncludePatterns() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("matchIncludePatterns() = %v, want %v", got, want)
		}
	}
}

func TestLoadWorktreeIncludePatterns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := "# secrets\n\n.env\n*.local\n"
	if err := os.WriteFile(filepath.Join(root, ".worktreeinclude"), []byte(content), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := loadWorktreeIncludePatterns(root)
	if err != nil {
		t.Fatalf("loadWorktreeIncludePatterns: %v", err)
	}
	want := []string{".env", "*.local"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("patterns = %v, want %v", got, want)
	}
}

func TestLoadWorktreeIncludePatterns_MissingFile(t *testing.T) {
	t.Parallel()

	got, err := loadWorktreeIncludePatterns(t.TempDir())
	if err != nil {
		t.Fatalf("loadWorktreeIncludePatterns: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("patterns = %v, want none", got)
	}
}

func TestCopyWorktreeIncludeFiles(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", testEnvFile+"\n")
	testutil.WriteFile(t, repoDir, ".worktreeinclude", testEnvFile+"\n")
	testutil.WriteFile(t, repoDir, testEnvFile, "SECRET=1\n")
	testutil.WriteFile(t, repoDir, "sub/"+testEnvFile, "SECRET=2\n")
	testutil.GitAdd(t, repoDir, ".gitignore", ".worktreeinclude")
	testutil.GitCommit(t, repoDir, "init")

	dest := t.TempDir()
	var errOut bytes.Buffer
	if err := copyWorktreeIncludeFiles(context.Background(), &errOut, repoDir, dest); err != nil {
		t.Fatalf("copyWorktreeIncludeFiles: %v; stderr: %s", err, errOut.String())
	}
	for _, rel := range []string{testEnvFile, "sub/" + testEnvFile} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("copied file %s missing: %v", rel, err)
		}
	}
}

func TestCopyWorktreeIncludeFiles_NoIncludeFileCopiesNothing(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", testEnvFile+"\n")
	testutil.WriteFile(t, repoDir, testEnvFile, "SECRET=1\n")

	dest := t.TempDir()
	var errOut bytes.Buffer
	if err := copyWorktreeIncludeFiles(context.Background(), &errOut, repoDir, dest); err != nil {
		t.Fatalf("copyWorktreeIncludeFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, testEnvFile)); !os.IsNotExist(err) {
		t.Fatalf("%s stat = %v, want not exist", testEnvFile, err)
	}
}

func TestCopyWorktreeIncludeFiles_SkipsSymlinkWithWarning(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)

	const symlinkPath = "link.env"
	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, ".gitignore", symlinkPath+"\ntarget.txt\n")
	testutil.WriteFile(t, repoDir, ".worktreeinclude", symlinkPath+"\n")
	testutil.WriteFile(t, repoDir, "target.txt", "x\n")
	if err := os.Symlink(filepath.Join(repoDir, "target.txt"), filepath.Join(repoDir, symlinkPath)); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	dest := t.TempDir()
	var errOut bytes.Buffer
	if err := copyWorktreeIncludeFiles(context.Background(), &errOut, repoDir, dest); err != nil {
		t.Fatalf("copyWorktreeIncludeFiles: %v", err)
	}
	if !strings.Contains(errOut.String(), "warning: skipped "+symlinkPath) {
		t.Fatalf("stderr = %q, want skip warning for %s", errOut.String(), symlinkPath)
	}
	if _, err := os.Stat(filepath.Join(dest, symlinkPath)); !os.IsNotExist(err) {
		t.Fatalf("%s stat = %v, want not exist", symlinkPath, err)
	}
}
