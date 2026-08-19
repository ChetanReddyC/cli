package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// ErrLinkedSubmoduleHooksUnsupported prevents hook installation in Git's
// internal submodule storage. Codex currently derives that unsafe location for
// linked submodules, so there is no project hook root Entire can safely manage.
var ErrLinkedSubmoduleHooksUnsupported = errors.New("codex hooks are not supported from a linked submodule")

// HookLocation describes the hooks file Codex actually discovers for the
// current checkout and any obsolete worktree-local file Entire may migrate.
type HookLocation struct {
	HooksPath       string
	LegacyHooksPath string
	LockPath        string
	RepositoryWide  bool
}

// ProjectLayerExists reports whether Codex will construct a project config
// layer for this checkout. Linked worktrees need a local .codex directory even
// though hooks.json itself is loaded from the authoritative root.
func (l HookLocation) ProjectLayerExists() bool {
	projectDir := filepath.Dir(l.HooksPath)
	if l.LegacyHooksPath != "" {
		projectDir = filepath.Dir(l.LegacyHooksPath)
	}
	info, err := os.Stat(projectDir)
	return err == nil && info.IsDir()
}

// ResolveHookLocation resolves Codex's repository-authoritative hooks file.
func ResolveHookLocation(ctx context.Context) (HookLocation, error) {
	worktreeRoot, err := resolveWorktreeRoot(ctx)
	if err != nil {
		return HookLocation{}, err
	}
	return resolveHookLocation(worktreeRoot)
}

func resolveWorktreeRoot(ctx context.Context) (string, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		worktreeRoot, err = os.Getwd() //nolint:forbidigo // Preserve hook setup in non-repository test and bootstrap directories.
		if err != nil {
			return "", fmt.Errorf("resolve current directory: %w", err)
		}
	}
	worktreeRoot, err = canonicalPath(worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	return worktreeRoot, nil
}

func worktreeLocalHooksPath(ctx context.Context) (string, error) {
	worktreeRoot, err := resolveWorktreeRoot(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(worktreeRoot, ".codex", HooksFileName), nil
}

func resolveHookLocation(worktreeRoot string) (HookLocation, error) {
	worktreeRoot, err := canonicalPath(worktreeRoot)
	if err != nil {
		return HookLocation{}, fmt.Errorf("resolve worktree root: %w", err)
	}

	location := HookLocation{
		HooksPath: filepath.Join(worktreeRoot, ".codex", HooksFileName),
	}
	location.LockPath = location.HooksPath + ".lock"
	dotGitPath := filepath.Join(worktreeRoot, ".git")
	info, err := os.Stat(dotGitPath)
	if errors.Is(err, os.ErrNotExist) {
		return location, nil
	}
	if err != nil {
		return HookLocation{}, fmt.Errorf("stat .git path: %w", err)
	}
	if info.IsDir() {
		gitDir, resolveErr := canonicalPath(dotGitPath)
		if resolveErr != nil {
			return HookLocation{}, fmt.Errorf("resolve Git directory: %w", resolveErr)
		}
		location.LockPath = filepath.Join(gitDir, "entire-codex-hooks.lock")
		location.RepositoryWide = hasRegisteredLinkedWorktree(gitDir)
		return location, nil
	}

	gitDir, err := readGitDirFile(dotGitPath, worktreeRoot)
	if err != nil {
		return HookLocation{}, err
	}
	location.LockPath = filepath.Join(gitDir, "entire-codex-hooks.lock")
	worktreesDir := filepath.Dir(gitDir)
	if filepath.Base(worktreesDir) != "worktrees" {
		return location, nil
	}

	commonDir, err := readCommonDir(gitDir, filepath.Dir(worktreesDir))
	if err != nil {
		return HookLocation{}, err
	}
	expectedCommonDir, err := canonicalPath(filepath.Dir(worktreesDir))
	if err != nil {
		return HookLocation{}, fmt.Errorf("resolve expected common Git directory: %w", err)
	}
	if commonDir != expectedCommonDir {
		return HookLocation{}, fmt.Errorf("linked worktree common directory %q does not match Git directory %q", commonDir, expectedCommonDir)
	}
	if !looksLikeGitDir(commonDir) {
		return HookLocation{}, fmt.Errorf("linked worktree common directory %q is not a Git directory", commonDir)
	}
	location.LockPath = filepath.Join(commonDir, "entire-codex-hooks.lock")
	location.LegacyHooksPath = filepath.Join(worktreeRoot, ".codex", HooksFileName)

	authoritativeRoot, err := canonicalPath(filepath.Dir(commonDir))
	if err != nil {
		return HookLocation{}, fmt.Errorf("resolve Codex hook root: %w", err)
	}
	if isInsideGitMetadata(authoritativeRoot) {
		return location, fmt.Errorf("%w: refusing unsafe hook root %q", ErrLinkedSubmoduleHooksUnsupported, authoritativeRoot)
	}

	location.HooksPath = filepath.Join(authoritativeRoot, ".codex", HooksFileName)
	location.RepositoryWide = true
	return location, nil
}

func hasRegisteredLinkedWorktree(commonDir string) bool {
	entries, err := os.ReadDir(filepath.Join(commonDir, "worktrees"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return true
		}
	}
	return false
}

func readGitDirFile(dotGitPath, worktreeRoot string) (string, error) {
	data, err := os.ReadFile(dotGitPath) //nolint:gosec // dotGitPath is derived from the resolved worktree root.
	if err != nil {
		return "", fmt.Errorf("read .git file: %w", err)
	}
	line, _, _ := strings.Cut(string(data), "\n")
	value, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return "", errors.New(".git file has no gitdir prefix")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New(".git file has an empty gitdir")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(worktreeRoot, value)
	}
	resolved, err := canonicalPath(value)
	if err != nil {
		return "", fmt.Errorf("resolve gitdir: %w", err)
	}
	return resolved, nil
}

func readCommonDir(gitDir, fallback string) (string, error) {
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir")) //nolint:gosec // gitDir comes from the checked-out repository's .git file.
	if errors.Is(err, os.ErrNotExist) {
		return canonicalPath(fallback)
	}
	if err != nil {
		return "", fmt.Errorf("read commondir file: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("commondir file is empty")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(gitDir, value)
	}
	resolved, err := canonicalPath(value)
	if err != nil {
		return "", fmt.Errorf("resolve common Git directory: %w", err)
	}
	return resolved, nil
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Clean(abs), nil
	}
	return "", fmt.Errorf("evaluate path symlinks: %w", err)
}

func isInsideGitMetadata(path string) bool {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		base := filepath.Base(current)
		if base == ".git" || base == ".bare" || looksLikeGitDir(current) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func looksLikeGitDir(path string) bool {
	head, headErr := os.Stat(filepath.Join(path, "HEAD"))
	objects, objectsErr := os.Stat(filepath.Join(path, "objects"))
	return headErr == nil && !head.IsDir() && objectsErr == nil && objects.IsDir()
}
