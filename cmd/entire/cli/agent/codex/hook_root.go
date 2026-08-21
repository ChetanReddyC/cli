package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

const maxGitMetadataFileBytes = 64 * 1024

// HookLocation describes the hooks file Codex actually discovers for the
// current checkout and any obsolete worktree-local file Entire may migrate.
type HookLocation struct {
	HooksPath       string
	LegacyHooksPath string
	LockPath        string
	RepositoryWide  bool
}

// UnsupportedHookLocationError carries the safe paths Entire may still use
// when Codex derives its shared hook root from unsupported Git metadata.
type UnsupportedHookLocationError struct {
	Location HookLocation
	HookRoot string
}

func (e *UnsupportedHookLocationError) Error() string {
	return fmt.Sprintf("codex hooks are not supported for derived hook root %q", e.HookRoot)
}

// HookInstallationSkipped marks this permanent safety refusal as non-fatal to
// setup for other selected agents.
func (e *UnsupportedHookLocationError) HookInstallationSkipped() {}

// ProjectLayerExists reports whether Codex will construct a project config
// layer for this checkout. Linked worktrees need a local .codex directory even
// though hooks.json itself is loaded from the authoritative root.
func (l HookLocation) ProjectLayerExists() bool {
	projectDir := filepath.Dir(l.HooksPath)
	if l.LegacyHooksPath != "" {
		projectDir = filepath.Dir(l.LegacyHooksPath)
	}
	return projectLayerExists(projectDir)
}

// WorktreeProjectLayerExists reports whether the current checkout has the
// local .codex directory Codex needs to construct its project config layer.
func WorktreeProjectLayerExists(ctx context.Context) bool {
	path, err := worktreeLocalHooksPath(ctx)
	return err == nil && projectLayerExists(filepath.Dir(path))
}

func projectLayerExists(projectDir string) bool {
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
	if isUserHookRoot(worktreeRoot) {
		return HookLocation{}, &UnsupportedHookLocationError{
			Location: location,
			HookRoot: worktreeRoot,
		}
	}
	dotGitPath := filepath.Join(worktreeRoot, ".git")
	info, err := os.Lstat(dotGitPath)
	if errors.Is(err, os.ErrNotExist) {
		return validateHookLocation(location)
	}
	if err != nil {
		return HookLocation{}, fmt.Errorf("stat .git path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return validateHookLocation(location)
	}
	if info.IsDir() {
		gitDir, resolveErr := canonicalPath(dotGitPath)
		if resolveErr != nil {
			return HookLocation{}, fmt.Errorf("resolve Git directory: %w", resolveErr)
		}
		location.LockPath = filepath.Join(gitDir, "entire-codex-hooks.lock")
		location.RepositoryWide = hasRegisteredLinkedWorktree(gitDir)
		return validateHookLocation(location)
	}
	if !info.Mode().IsRegular() {
		return validateHookLocation(location)
	}

	gitDir, err := readGitDirFile(dotGitPath, worktreeRoot)
	if err != nil {
		return validateHookLocation(location)
	}
	commonDir, authoritativeRoot, ok := resolveSharedHookRoot(gitDir, worktreeRoot)
	if !ok {
		return validateHookLocation(location)
	}
	location.LockPath = filepath.Join(commonDir, "entire-codex-hooks.lock")
	location.LegacyHooksPath = filepath.Join(worktreeRoot, ".codex", HooksFileName)
	if isInsideGitMetadata(authoritativeRoot) || isUserHookRoot(authoritativeRoot) {
		if _, validateErr := resolveHookDestination(location.LegacyHooksPath); validateErr != nil {
			return HookLocation{}, validateErr
		}
		return HookLocation{}, &UnsupportedHookLocationError{
			Location: location,
			HookRoot: authoritativeRoot,
		}
	}

	location.HooksPath = filepath.Join(authoritativeRoot, ".codex", HooksFileName)
	location.RepositoryWide = true
	return validateHookLocation(location)
}

// Codex falls back to worktree-local config unless both the linked checkout
// and candidate root prove ownership of the common Git directory.
func resolveSharedHookRoot(gitDir, worktreeRoot string) (commonDir, authoritativeRoot string, ok bool) {
	worktreesDir := filepath.Dir(gitDir)
	if filepath.Base(worktreesDir) != "worktrees" {
		return "", "", false
	}
	commonDir, err := readCommonDir(gitDir)
	if err != nil {
		return "", "", false
	}
	expectedCommonDir, err := canonicalPath(filepath.Dir(worktreesDir))
	if err != nil || commonDir != expectedCommonDir || !looksLikeGitDir(commonDir) {
		return "", "", false
	}
	authoritativeRoot, err = canonicalPath(filepath.Dir(commonDir))
	if err != nil {
		return "", "", false
	}
	if !worktreeRegistrationValid(gitDir, worktreeRoot) || !ownsCommonDir(authoritativeRoot, commonDir) {
		return "", "", false
	}
	return commonDir, authoritativeRoot, true
}

// The registration backlink proves that Git recognizes this checkout.
func worktreeRegistrationValid(gitDir, worktreeRoot string) bool {
	data, err := readGitMetadataFile(filepath.Join(gitDir, "gitdir"))
	if err != nil {
		return false
	}
	target := strings.TrimSpace(string(data))
	if target == "" || filepath.Base(target) != ".git" {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(gitDir, target)
	}
	registeredRoot, err := canonicalPath(filepath.Dir(target))
	return err == nil && registeredRoot == worktreeRoot
}

// The candidate root owns the common directory only through its own .git entry.
func ownsCommonDir(candidateRoot, commonDir string) bool {
	dotGit := filepath.Join(candidateRoot, ".git")
	info, err := os.Lstat(dotGit)
	if err != nil {
		return false
	}
	if info.IsDir() {
		resolved, resolveErr := canonicalPath(dotGit)
		return resolveErr == nil && resolved == commonDir
	}
	if !info.Mode().IsRegular() {
		return false
	}
	resolved, err := readGitDirFile(dotGit, candidateRoot)
	return err == nil && resolved == commonDir
}

func isUserHookRoot(hookRoot string) bool {
	home, err := os.UserHomeDir()
	if err == nil {
		canonicalHome, canonicalErr := canonicalPath(home)
		if canonicalErr == nil && hookRoot == canonicalHome {
			return true
		}
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		if home == "" {
			return false
		}
		codexHome = filepath.Join(home, ".codex")
	}
	canonicalCodexHome, err := canonicalPath(codexHome)
	if err != nil {
		return false
	}
	canonicalProjectDir, err := canonicalPath(filepath.Join(hookRoot, ".codex"))
	return err == nil && canonicalProjectDir == canonicalCodexHome
}

func omitAliasedLegacyHooks(location HookLocation) (HookLocation, error) {
	if location.LegacyHooksPath == "" || filepath.Base(location.HooksPath) != filepath.Base(location.LegacyHooksPath) {
		return location, nil
	}
	authoritativeDir, err := os.Stat(filepath.Dir(location.HooksPath))
	if errors.Is(err, os.ErrNotExist) {
		return location, nil
	}
	if err != nil {
		return HookLocation{}, fmt.Errorf("stat authoritative Codex directory: %w", err)
	}
	legacyDir, err := os.Stat(filepath.Dir(location.LegacyHooksPath))
	if errors.Is(err, os.ErrNotExist) {
		return location, nil
	}
	if err != nil {
		return HookLocation{}, fmt.Errorf("stat legacy Codex directory: %w", err)
	}
	if os.SameFile(authoritativeDir, legacyDir) {
		location.LegacyHooksPath = ""
	}
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
	data, err := readGitMetadataFile(dotGitPath)
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
	info, err := os.Lstat(value)
	if err != nil {
		return "", fmt.Errorf("inspect gitdir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("gitdir %q is not a directory", value)
	}
	resolved, err := canonicalPath(value)
	if err != nil {
		return "", fmt.Errorf("resolve gitdir: %w", err)
	}
	return resolved, nil
}

func readCommonDir(gitDir string) (string, error) {
	data, err := readGitMetadataFile(filepath.Join(gitDir, "commondir"))
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

func readGitMetadataFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect git metadata file %q: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("git metadata path %q is not a regular file", path)
	}
	if before.Size() > maxGitMetadataFileBytes {
		return nil, fmt.Errorf("git metadata file %q exceeds %d bytes", path, maxGitMetadataFileBytes)
	}
	file, err := os.Open(path) //nolint:gosec // Callers derive paths from the current repository's Git metadata.
	if err != nil {
		return nil, fmt.Errorf("open git metadata file %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened git metadata file %q: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("git metadata file %q changed while opening", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxGitMetadataFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read git metadata file %q: %w", path, err)
	}
	if len(data) > maxGitMetadataFileBytes {
		return nil, fmt.Errorf("git metadata file %q exceeds %d bytes", path, maxGitMetadataFileBytes)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("reinspect git metadata file %q: %w", path, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fmt.Errorf("git metadata file %q changed while reading", path)
	}
	return data, nil
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
