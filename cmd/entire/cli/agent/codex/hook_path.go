package codex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

type hookDestination struct {
	path        string
	fileSymlink bool
}

func validateHookLocation(location HookLocation) (HookLocation, error) {
	location, err := omitAliasedLegacyHooks(location)
	if err != nil {
		return HookLocation{}, err
	}
	if location.HooksPath != "" {
		if _, err := resolveHookDestination(location.HooksPath); err != nil {
			return HookLocation{}, err
		}
	}
	if location.LegacyHooksPath != "" {
		if _, err := resolveHookDestination(location.LegacyHooksPath); err != nil {
			return HookLocation{}, err
		}
	}
	return location, nil
}

func resolveHookDestination(hooksPath string) (hookDestination, error) {
	absPath, err := filepath.Abs(hooksPath)
	if err != nil {
		return hookDestination{}, fmt.Errorf("make Codex hooks path absolute: %w", err)
	}
	absPath = filepath.Clean(absPath)
	projectDir := filepath.Dir(absPath)
	if filepath.Base(absPath) != HooksFileName || filepath.Base(projectDir) != ".codex" {
		return hookDestination{}, fmt.Errorf("invalid repository Codex hooks path %q", hooksPath)
	}

	checkoutRoot, err := canonicalPath(filepath.Dir(projectDir))
	if err != nil {
		return hookDestination{}, fmt.Errorf("resolve Codex hooks checkout: %w", err)
	}
	resolvedProjectDir, err := resolveProjectDir(projectDir, checkoutRoot)
	if err != nil {
		return hookDestination{}, err
	}

	info, err := os.Lstat(absPath)
	if errors.Is(err, os.ErrNotExist) {
		return hookDestination{path: filepath.Join(resolvedProjectDir, HooksFileName)}, nil
	}
	if err != nil {
		return hookDestination{}, fmt.Errorf("inspect Codex hooks file: %w", err)
	}

	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return hookDestination{}, fmt.Errorf("resolve Codex hooks file: %w", err)
	}
	if !paths.IsSubpath(checkoutRoot, resolvedPath) {
		return hookDestination{}, outsideCheckoutError(absPath, checkoutRoot, resolvedPath)
	}
	resolvedInfo, err := os.Stat(resolvedPath)
	if err != nil {
		return hookDestination{}, fmt.Errorf("inspect resolved Codex hooks file: %w", err)
	}
	if resolvedInfo.IsDir() {
		return hookDestination{}, fmt.Errorf("codex hooks path %q resolves to a directory", absPath)
	}
	return hookDestination{
		path:        resolvedPath,
		fileSymlink: info.Mode()&os.ModeSymlink != 0,
	}, nil
}

func resolveProjectDir(projectDir, checkoutRoot string) (string, error) {
	_, err := os.Lstat(projectDir)
	if errors.Is(err, os.ErrNotExist) {
		return filepath.Join(checkoutRoot, ".codex"), nil
	}
	if err != nil {
		return "", fmt.Errorf("inspect repository .codex directory: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(projectDir)
	if err != nil {
		return "", fmt.Errorf("resolve repository .codex directory: %w", err)
	}
	if !paths.IsSubpath(checkoutRoot, resolved) {
		return "", outsideCheckoutError(projectDir, checkoutRoot, resolved)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect resolved repository .codex directory: %w", err)
	}
	if !resolvedInfo.IsDir() {
		return "", fmt.Errorf("repository .codex path %q is not a directory", projectDir)
	}
	return resolved, nil
}

func outsideCheckoutError(path, checkoutRoot, resolved string) error {
	return fmt.Errorf("codex hooks path %q resolves outside the checkout %q to %q", path, checkoutRoot, resolved)
}
