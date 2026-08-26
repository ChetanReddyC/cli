package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func validateWorktreeHookTarget(hooks WorktreeHooksPath) (string, error) {
	if hooks.worktreeRoot == "" || hooks.path == "" {
		return "", errors.New("invalid empty worktree Codex hooks path")
	}
	rootInfo, err := os.Stat(hooks.worktreeRoot)
	if err != nil {
		return "", fmt.Errorf("inspect Codex hooks checkout: %w", err)
	}
	if !rootInfo.IsDir() {
		return "", fmt.Errorf("codex hooks checkout %q is not a directory", hooks.worktreeRoot)
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
	if !info.IsDir() {
		return fmt.Errorf("repository .codex path %q is not a directory", projectDir)
	}
	return nil
}
