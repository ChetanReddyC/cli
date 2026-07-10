package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
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

func newTrailWorktreeTestRepo(t *testing.T) string {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	repoDir := t.TempDir()
	testutil.InitRepo(t, repoDir)
	testutil.WriteFile(t, repoDir, "README.md", "test\n")
	testutil.GitAdd(t, repoDir, "README.md")
	testutil.GitCommit(t, repoDir, "initial")
	return repoDir
}

func runTrailWorktreeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
}

func currentBranchInDir(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", "branch", "--show-current")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git branch --show-current failed: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func TestCheckoutTrailWorktree_CreatesWorktree(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runTrailWorktreeGit(t, repoDir, "branch", "feature/test")
	testutil.WriteFile(t, repoDir, ".worktreeinclude", ".env\n")
	testutil.WriteFile(t, repoDir, ".env", "SECRET=1\n")
	testutil.WriteFile(t, repoDir, ".gitignore", ".env\n")
	testutil.GitAdd(t, repoDir, ".worktreeinclude", ".gitignore")
	testutil.GitCommit(t, repoDir, "add include config")
	startBranch := currentBranchInDir(t, repoDir)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/test", false, 7); err != nil {
		t.Fatalf("checkoutTrailWorktree: %v; stderr: %s", err, errOut.String())
	}

	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-7-feature-test")
	if !strings.Contains(out.String(), "cd "+shellQuotePath(wantPath)) {
		t.Fatalf("output = %q, want cd hint for %q", out.String(), wantPath)
	}
	if got := currentBranchInDir(t, repoDir); got != startBranch {
		t.Fatalf("current branch = %q, want unchanged %q", got, startBranch)
	}
	if got := currentBranchInDir(t, wantPath); got != "feature/test" {
		t.Fatalf("worktree branch = %q, want feature/test", got)
	}
	if _, err := os.Stat(filepath.Join(wantPath, ".env")); err != nil {
		t.Fatalf(".worktreeinclude copy missing: %v", err)
	}
	excludeContent, err := os.ReadFile(filepath.Join(repoDir, ".git", "info", "exclude"))
	if err != nil {
		t.Fatalf("read exclude: %v", err)
	}
	if !strings.Contains(string(excludeContent), ".entire/worktrees/") {
		t.Fatalf("exclude = %q, want .entire/worktrees/ rule", string(excludeContent))
	}
}

func TestCheckoutTrailWorktree_FromLinkedWorktreeCreatesSibling(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runTrailWorktreeGit(t, repoDir, "branch", "feature/first")
	runTrailWorktreeGit(t, repoDir, "branch", "feature/second")
	t.Chdir(repoDir)

	var out1, err1 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out1, &err1, "feature/first", false, 7); err != nil {
		t.Fatalf("first checkout: %v; stderr: %s", err, err1.String())
	}
	firstPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-7-feature-first")
	t.Chdir(firstPath)

	var out2, err2 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out2, &err2, "feature/second", false, 8); err != nil {
		t.Fatalf("second checkout: %v; stderr: %s", err, err2.String())
	}

	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-8-feature-second")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("sibling worktree missing: %v", err)
	}
	nested := filepath.Join(firstPath, ".entire", "worktrees", "trail-8-feature-second")
	if _, err := os.Stat(nested); !os.IsNotExist(err) {
		t.Fatalf("nested worktree stat = %v, want not exist", err)
	}
}

func TestCheckoutTrailWorktree_ReusesExistingWorktree(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	runTrailWorktreeGit(t, repoDir, "branch", "feature/reuse")
	t.Chdir(repoDir)

	var out1, err1 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out1, &err1, "feature/reuse", false, 9); err != nil {
		t.Fatalf("first checkout: %v; stderr: %s", err, err1.String())
	}
	var out2, err2 bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out2, &err2, "feature/reuse", false, 9); err != nil {
		t.Fatalf("second checkout: %v; stderr: %s", err, err2.String())
	}
	if !strings.Contains(out2.String(), "Worktree already exists") {
		t.Fatalf("second output = %q, want existing-worktree message", out2.String())
	}
}

func TestCheckoutTrailWorktree_FetchesRemoteOnlyBranch(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	paths.ClearWorktreeRootCache()
	t.Cleanup(paths.ClearWorktreeRootCache)

	tmp := t.TempDir()
	originDir := filepath.Join(tmp, "origin.git")
	seedDir := filepath.Join(tmp, "seed")
	repoDir := filepath.Join(tmp, "local")
	runTrailWorktreeGit(t, tmp, "init", "--bare", originDir)
	testutil.InitRepo(t, seedDir)
	testutil.WriteFile(t, seedDir, "README.md", "test\n")
	testutil.GitAdd(t, seedDir, "README.md")
	testutil.GitCommit(t, seedDir, "initial")
	runTrailWorktreeGit(t, seedDir, "checkout", "-b", "feature/remote")
	testutil.WriteFile(t, seedDir, "remote.txt", "remote\n")
	testutil.GitAdd(t, seedDir, "remote.txt")
	testutil.GitCommit(t, seedDir, "remote branch")
	runTrailWorktreeGit(t, seedDir, "remote", "add", "origin", originDir)
	runTrailWorktreeGit(t, seedDir, "push", "origin", "--all")
	runTrailWorktreeGit(t, tmp, "clone", originDir, repoDir)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	if err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/remote", false, 12); err != nil {
		t.Fatalf("checkoutTrailWorktree: %v; stderr: %s", err, errOut.String())
	}

	wantPath := filepath.Join(repoDir, ".entire", "worktrees", "trail-12-feature-remote")
	if got := currentBranchInDir(t, wantPath); got != "feature/remote" {
		t.Fatalf("worktree branch = %q, want feature/remote", got)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "remote.txt")); err != nil {
		t.Fatalf("remote branch file missing: %v", err)
	}
}

func TestCheckoutTrailWorktree_RejectsInvalidBranch(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out, &errOut, "-bad", false, 1)
	if err == nil || !strings.Contains(err.Error(), "invalid branch") {
		t.Fatalf("error = %v, want invalid branch", err)
	}
}

func TestCheckoutTrailWorktree_UnknownBranch(t *testing.T) {
	repoDir := newTrailWorktreeTestRepo(t)
	t.Chdir(repoDir)

	var out, errOut bytes.Buffer
	err := checkoutTrailWorktree(context.Background(), &out, &errOut, "feature/nope", false, 3)
	if err == nil || !strings.Contains(err.Error(), "not found locally or on origin") {
		t.Fatalf("error = %v, want branch-not-found", err)
	}
}
