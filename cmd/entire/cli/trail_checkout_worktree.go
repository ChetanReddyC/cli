package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	huh "charm.land/huh/v2"
	"github.com/go-git/go-git/v6/plumbing/format/gitignore"

	"github.com/entireio/cli/cmd/entire/cli/interactive"
)

const (
	trailWorktreesRelDir      = ".entire/worktrees"
	trailWorktreeFallbackName = "branch"
)

func defaultTrailWorktreePath(repoRoot, branch string, trailNumber int) string {
	name := sanitizeTrailWorktreeName(branch)
	if trailNumber > 0 {
		name = fmt.Sprintf("trail-%d-%s", trailNumber, name)
	}
	return filepath.Join(repoRoot, filepath.FromSlash(trailWorktreesRelDir), name)
}

func sanitizeTrailWorktreeName(branch string) string {
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, strings.TrimSpace(branch))
	name = strings.Trim(name, "-.")
	if name == "" {
		return trailWorktreeFallbackName
	}
	return name
}

func shellQuotePath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
}

// gitCommonDirForTrailWorktree returns the absolute git common dir, which is
// the main repo's .git directory even when run from a linked worktree.
func gitCommonDirForTrailWorktree(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-common-dir")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git common dir: %w", err)
	}
	gitDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(gitDir) {
		cwd, wdErr := os.Getwd() //nolint:forbidigo // must resolve relative git common dir in cwd context
		if wdErr != nil {
			return "", fmt.Errorf("failed to get current directory: %w", wdErr)
		}
		gitDir = filepath.Join(cwd, gitDir)
	}
	return filepath.Clean(gitDir), nil
}

func trailWorktreeBaseRoot(ctx context.Context) (string, error) { //nolint:unused // used by later tasks
	gitDir, err := gitCommonDirForTrailWorktree(ctx)
	if err != nil {
		return "", err
	}
	if filepath.Base(gitDir) != ".git" {
		return "", fmt.Errorf("git common dir %q is not a .git directory", gitDir)
	}
	return filepath.Dir(gitDir), nil
}

// ensureTrailWorktreeIgnoreRule makes sure .entire/worktrees/ is git-ignored.
// Already ignored (any mechanism) → silent no-op. Interactively it offers the
// shared .gitignore first; declining, aborting, --force, or a non-TTY all fall
// back to the local-only .git/info/exclude. Either write prints a notice.
func ensureTrailWorktreeIgnoreRule(ctx context.Context, w io.Writer, root string, force bool) error {
	check := exec.CommandContext(ctx, "git", "check-ignore", "-q", trailWorktreesRelDir+"/")
	check.Dir = root
	err := check.Run()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return fmt.Errorf("failed to check ignore status of %s: %w", trailWorktreesRelDir, err)
	}

	const rule = trailWorktreesRelDir + "/"
	useGitignore := false
	if !force && interactive.CanPromptInteractively() {
		confirmed := true
		form := NewAccessibleForm(
			huh.NewGroup(
				huh.NewConfirm().
					Title("Add .entire/worktrees/ to .gitignore?").
					Description("Choosing No adds a local-only rule to .git/info/exclude instead.").
					Value(&confirmed),
			),
		)
		if err := form.Run(); err != nil {
			if !errors.Is(err, huh.ErrUserAborted) {
				return fmt.Errorf("failed to get confirmation: %w", err)
			}
			confirmed = false
		}
		useGitignore = confirmed
	}

	if useGitignore {
		if err := appendIgnoreRule(filepath.Join(root, ".gitignore"), rule); err != nil {
			return err
		}
		fmt.Fprintln(w, "Added .entire/worktrees/ to .gitignore — commit this when convenient.")
		return nil
	}
	gitDir, err := gitCommonDirForTrailWorktree(ctx)
	if err != nil {
		return err
	}
	if err := appendIgnoreRule(filepath.Join(gitDir, "info", "exclude"), rule); err != nil {
		return err
	}
	fmt.Fprintln(w, "Added .entire/worktrees/ to .git/info/exclude (local to this clone).")
	return nil
}

func appendIgnoreRule(path, rule string) error { //nolint:unparam // rule parameter defined in brief signature
	content, err := os.ReadFile(path) //nolint:gosec // path derived from repo root / git common dir
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to read %s: %w", path, err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		if strings.TrimSpace(line) == rule {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("failed to create %s: %w", filepath.Dir(path), err)
	}
	prefix := ""
	if len(content) > 0 && !strings.HasSuffix(string(content), "\n") {
		prefix = "\n"
	}
	updated := string(content) + prefix + rule + "\n"
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil { //nolint:gosec // path derived from repo root / git common dir
		return fmt.Errorf("failed to update %s: %w", path, err)
	}
	return nil
}

const worktreeIncludeFile = ".worktreeinclude"

// copyWorktreeIncludeFiles copies ignored files matching .worktreeinclude
// patterns from the main worktree root into a freshly created worktree.
// Per-file failures warn and skip; they never fail the checkout.
func copyWorktreeIncludeFiles(ctx context.Context, errW io.Writer, root, dest string) error {
	patterns, err := loadWorktreeIncludePatterns(root)
	if err != nil {
		return err
	}
	if len(patterns) == 0 {
		return nil
	}
	ignored, err := listIgnoredFiles(ctx, root)
	if err != nil {
		return err
	}
	for _, rel := range matchIncludePatterns(patterns, ignored) {
		if err := copyIncludedFile(filepath.Join(root, rel), filepath.Join(dest, rel)); err != nil {
			fmt.Fprintf(errW, "warning: skipped %s: %v\n", filepath.ToSlash(rel), err)
		}
	}
	return nil
}

// loadWorktreeIncludePatterns reads .worktreeinclude from root. A missing
// file means nothing gets copied. Lines are gitignore-style patterns; blank
// lines and #-comments are skipped.
func loadWorktreeIncludePatterns(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, worktreeIncludeFile)) //nolint:gosec // path derived from repo root
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", worktreeIncludeFile, err)
	}
	var patterns []string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, nil
}

// listIgnoredFiles returns untracked files ignored by repo ignore rules,
// relative to root.
func listIgnoredFiles(ctx context.Context, root string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	cmd.Dir = root
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list ignored files: %w", err)
	}
	var files []string
	for _, f := range bytes.Split(output, []byte{0}) {
		if len(f) > 0 {
			files = append(files, string(f))
		}
	}
	return files, nil
}

func matchIncludePatterns(patterns, files []string) []string {
	ps := make([]gitignore.Pattern, 0, len(patterns))
	for _, pattern := range patterns {
		ps = append(ps, gitignore.ParsePattern(pattern, nil))
	}
	matcher := gitignore.NewMatcher(ps)
	included := make([]string, 0, len(files))
	for _, file := range files {
		rel, ok := cleanRelativeIncludeFile(file)
		if !ok || isManagedTrailWorktreePath(rel) || !matcher.Match(strings.Split(filepath.ToSlash(rel), "/"), false) {
			continue
		}
		included = append(included, rel)
	}
	return included
}

func isManagedTrailWorktreePath(rel string) bool {
	slash := filepath.ToSlash(rel)
	return slash == trailWorktreesRelDir || strings.HasPrefix(slash, trailWorktreesRelDir+"/")
}

func cleanRelativeIncludeFile(rel string) (string, bool) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	return clean, true
}

func copyIncludedFile(src, dst string) error {
	srcInfo, err := os.Lstat(src)
	if err != nil {
		return err //nolint:wrapcheck // lstat error is sufficient for caller context
	}
	if !srcInfo.Mode().IsRegular() {
		return errors.New("source is not a regular file")
	}
	in, err := os.Open(src) //nolint:gosec // src derived from repo root + .worktreeinclude
	if err != nil {
		return err //nolint:wrapcheck // open error is sufficient for caller context
	}
	defer in.Close()
	openedInfo, err := in.Stat()
	if err != nil {
		return err //nolint:wrapcheck // stat error is sufficient for caller context
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(srcInfo, openedInfo) {
		return errors.New("source changed while opening")
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err //nolint:wrapcheck // mkdir error is sufficient for caller context
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, srcInfo.Mode().Perm()) //nolint:gosec // dst is inside the new worktree
	if err != nil {
		return err //nolint:wrapcheck // openfile error is sufficient for caller context
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err //nolint:wrapcheck // copy error is sufficient for caller context
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err //nolint:wrapcheck // close error is sufficient for caller context
	}
	if err := os.Chmod(dst, srcInfo.Mode().Perm()); err != nil {
		_ = os.Remove(dst)
		return err //nolint:wrapcheck // chmod error is sufficient for caller context
	}
	return nil
}
