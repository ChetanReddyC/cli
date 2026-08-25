package codex

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type hookDestination struct {
	path   string
	exists bool
	mode   fs.FileMode
}

func resolveHookDestination(hooks WorktreeHooksPath) (hookDestination, error) {
	projectDir, err := validateWorktreeHookTarget(hooks)
	if err != nil {
		return hookDestination{}, err
	}
	if err := validateExistingProjectDir(projectDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return hookDestination{path: hooks.path, mode: 0o600}, nil
		}
		return hookDestination{}, err
	}

	info, err := os.Lstat(hooks.path)
	if errors.Is(err, os.ErrNotExist) {
		return hookDestination{path: hooks.path, mode: 0o600}, nil
	}
	if err != nil {
		return hookDestination{}, fmt.Errorf("inspect Codex hooks file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return hookDestination{}, fmt.Errorf("codex hooks path %q is a symbolic link", hooks.path)
	}
	if !info.Mode().IsRegular() {
		return hookDestination{}, fmt.Errorf("codex hooks path %q is not a regular file", hooks.path)
	}
	resolvedPath, err := filepath.EvalSymlinks(hooks.path)
	if err != nil {
		return hookDestination{}, fmt.Errorf("resolve Codex hooks file: %w", err)
	}
	if filepath.Clean(resolvedPath) != hooks.path {
		return hookDestination{}, fmt.Errorf(
			"codex hooks path %q resolves to unrelated path %q",
			hooks.path,
			resolvedPath,
		)
	}
	resolvedInfo, err := os.Stat(resolvedPath)
	if err != nil {
		return hookDestination{}, fmt.Errorf("inspect resolved Codex hooks file: %w", err)
	}
	if !resolvedInfo.Mode().IsRegular() || !os.SameFile(info, resolvedInfo) {
		return hookDestination{}, fmt.Errorf("codex hooks path %q changed while validating", hooks.path)
	}

	return hookDestination{
		path:   hooks.path,
		exists: true,
		mode:   info.Mode().Perm(),
	}, nil
}

func worktreeHooksMayExist(hooks WorktreeHooksPath) (bool, error) {
	projectDir, err := validateWorktreeHookTarget(hooks)
	if err != nil {
		return false, err
	}
	if err := validateExistingProjectDir(projectDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if _, err := os.Lstat(hooks.path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect Codex hooks file: %w", err)
	}
	return true, nil
}

func ensureWorktreeProjectDir(hooks WorktreeHooksPath) error {
	projectDir, err := validateWorktreeHookTarget(hooks)
	if err != nil {
		return err
	}
	if err := validateExistingProjectDir(projectDir); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	if err := os.Mkdir(projectDir, 0o750); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create repository .codex directory: %w", err)
		}
	}
	if err := validateExistingProjectDir(projectDir); err != nil {
		return err
	}
	return nil
}

func validateWorktreeHookTarget(hooks WorktreeHooksPath) (string, error) {
	if hooks.worktreeRoot == "" || hooks.path == "" {
		return "", errors.New("invalid empty worktree Codex hooks path")
	}
	canonicalRoot, err := canonicalPath(hooks.worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("resolve Codex hooks checkout: %w", err)
	}
	if canonicalRoot != hooks.worktreeRoot {
		return "", fmt.Errorf(
			"codex hooks checkout %q resolves to unexpected path %q",
			hooks.worktreeRoot,
			canonicalRoot,
		)
	}
	rootInfo, err := os.Lstat(hooks.worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("inspect Codex hooks checkout: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", fmt.Errorf("codex hooks checkout %q is not a canonical directory", hooks.worktreeRoot)
	}

	projectDir := filepath.Join(hooks.worktreeRoot, ".codex")
	expectedPath := filepath.Join(projectDir, HooksFileName)
	if hooks.path != expectedPath {
		return "", fmt.Errorf(
			"invalid worktree Codex hooks path %q: expected %q",
			hooks.path,
			expectedPath,
		)
	}
	return projectDir, nil
}

func validateDiscoveredHookTarget(hooks DiscoveredHooksPath) error {
	if hooks.path == "" {
		return errors.New("invalid empty discovered Codex hooks path")
	}
	root := filepath.Dir(filepath.Dir(hooks.path))
	canonicalRoot, err := canonicalPath(root)
	if err != nil {
		return fmt.Errorf("resolve Codex-discovered checkout: %w", err)
	}
	if canonicalRoot != root {
		return fmt.Errorf("codex-discovered checkout %q resolves to unexpected path %q", root, canonicalRoot)
	}
	expectedPath := filepath.Join(root, ".codex", HooksFileName)
	if hooks.path != expectedPath {
		return fmt.Errorf("invalid Codex-discovered hooks path %q: expected %q", hooks.path, expectedPath)
	}
	if err := validateExistingProjectDir(filepath.Dir(hooks.path)); err != nil {
		return err
	}
	return nil
}

func validateExistingProjectDir(projectDir string) error {
	info, err := os.Lstat(projectDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("inspect repository .codex directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("repository .codex path %q is a redirected directory", projectDir)
	}
	if !info.IsDir() {
		return fmt.Errorf("repository .codex path %q is not a directory", projectDir)
	}

	resolved, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return fmt.Errorf("resolve repository .codex directory: %w", err)
	}
	if filepath.Clean(resolved) != projectDir {
		return fmt.Errorf(
			"repository .codex path %q resolves to unexpected directory %q",
			projectDir,
			resolved,
		)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("inspect resolved repository .codex directory: %w", err)
	}
	if !resolvedInfo.IsDir() || !os.SameFile(info, resolvedInfo) {
		return fmt.Errorf("repository .codex path %q changed while validating", projectDir)
	}
	return nil
}
