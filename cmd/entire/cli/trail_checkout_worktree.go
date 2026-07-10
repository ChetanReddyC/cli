package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	huh "charm.land/huh/v2"

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
