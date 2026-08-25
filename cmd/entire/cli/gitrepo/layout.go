package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	gitconfig "github.com/go-git/go-git/v6/config"
)

// GitLayoutKind identifies how a checkout relates to its Git metadata.
type GitLayoutKind uint8

const (
	GitLayoutUnresolved GitLayoutKind = iota
	GitLayoutNormal
	GitLayoutLinkedWorktree
	GitLayoutBareWorktree
	GitLayoutSubmodule
	GitLayoutLinkedSubmodule
	GitLayoutSeparateGitDir
)

// GitLayout is the canonical, read-only description of a checkout's Git
// metadata. MainWorktreeRoot is populated only when the layout proves a
// checkout root that owns the shared Git directory.
type GitLayout struct {
	Kind               GitLayoutKind
	WorktreeRoot       string
	GitDir             string
	CommonDir          string
	MainWorktreeRoot   string
	WorktreeID         string
	HasLinkedWorktrees bool
}

var (
	gitLayoutCacheMu sync.RWMutex
	gitLayoutCache   = make(map[string]GitLayout)
)

// ResolveGitLayout resolves and memoizes the Git layout for the current
// checkout.
func ResolveGitLayout(ctx context.Context) (GitLayout, error) {
	worktreeRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		return GitLayout{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	return ResolveGitLayoutAt(worktreeRoot)
}

// ResolveGitLayoutAt resolves and memoizes the Git layout rooted at
// worktreeRoot.
func ResolveGitLayoutAt(worktreeRoot string) (GitLayout, error) {
	root, err := canonicalExistingPath(worktreeRoot)
	if err != nil {
		return GitLayout{}, fmt.Errorf("resolve worktree root: %w", err)
	}

	gitLayoutCacheMu.RLock()
	if cached, ok := gitLayoutCache[root]; ok {
		gitLayoutCacheMu.RUnlock()
		return cached, nil
	}
	gitLayoutCacheMu.RUnlock()

	layout, err := resolveGitLayout(root)
	if err != nil {
		return layout, err
	}
	gitLayoutCacheMu.Lock()
	gitLayoutCache[root] = layout
	gitLayoutCacheMu.Unlock()
	return layout, nil
}

// ClearGitLayoutCache clears memoized Git layouts. It is intended for tests
// that change repository metadata in-process.
func ClearGitLayoutCache() {
	gitLayoutCacheMu.Lock()
	clear(gitLayoutCache)
	gitLayoutCacheMu.Unlock()
}

func resolveGitLayout(worktreeRoot string) (GitLayout, error) {
	layout := GitLayout{Kind: GitLayoutUnresolved, WorktreeRoot: worktreeRoot}
	dotGitEntry := filepath.Join(worktreeRoot, gitDir)
	info, err := os.Lstat(dotGitEntry)
	if err != nil {
		return layout, fmt.Errorf("inspect .git path: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return layout, errors.New(".git path is a symbolic link")
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return layout, fmt.Errorf(".git path has unsupported mode %s", info.Mode())
	}

	dotGitPath, err := resolveDotGitPath(worktreeRoot)
	if err != nil {
		return layout, fmt.Errorf("resolve .git path: %w", err)
	}
	layout.GitDir, err = canonicalExistingPath(dotGitPath)
	if err != nil {
		return layout, fmt.Errorf("resolve Git directory: %w", err)
	}
	if !isGitCommonDir(layout.GitDir) && info.IsDir() {
		return layout, fmt.Errorf(".git directory %q is missing required Git metadata", layout.GitDir)
	}

	commonDir, err := resolveCommonGitPath(layout.GitDir)
	if err != nil {
		return layout, fmt.Errorf("resolve common Git directory: %w", err)
	}
	hasCommonDir := commonDir != ""
	if hasCommonDir {
		layout.CommonDir, err = canonicalExistingPath(commonDir)
		if err != nil {
			return layout, fmt.Errorf("resolve common Git directory: %w", err)
		}
	} else {
		layout.CommonDir = layout.GitDir
	}
	if !isGitCommonDir(layout.CommonDir) {
		return layout, fmt.Errorf("common Git directory %q is missing required Git metadata", layout.CommonDir)
	}

	if info.IsDir() {
		if hasCommonDir {
			return layout, errors.New("normal checkout has an unexpected commondir file")
		}
		layout.Kind = GitLayoutNormal
		layout.MainWorktreeRoot = worktreeRoot
		layout.HasLinkedWorktrees = hasRegisteredWorktrees(layout.CommonDir)
		return layout, nil
	}

	parsed := paths.ParseWorktreeGitDir(layout.GitDir)
	layout.WorktreeID = parsed.WorktreeID
	switch parsed.Layout {
	case paths.GitDirLayoutUnrecognized:
		if hasCommonDir {
			return layout, errors.New("separate Git directory has an unexpected commondir file")
		}
		layout.Kind = GitLayoutSeparateGitDir
		return layout, nil
	case paths.GitDirLayoutSubmodule:
		if hasCommonDir {
			return layout, errors.New("ordinary submodule has an unexpected commondir file")
		}
		layout.Kind = GitLayoutSubmodule
		return layout, nil
	case paths.GitDirLayoutLinkedWorktree:
		if err := validateLinkedLayout(layout, hasCommonDir); err != nil {
			return layout, err
		}
		if filepath.Base(layout.CommonDir) != gitDir {
			return layout, fmt.Errorf("linked worktree common directory %q is not named .git", layout.CommonDir)
		}
		bare, err := commonGitDirIsBare(layout.CommonDir)
		if err != nil {
			return layout, err
		}
		if bare {
			layout.Kind = GitLayoutBareWorktree
			return layout, nil
		}
		layout.MainWorktreeRoot = filepath.Dir(layout.CommonDir)
		if !rootOwnsGitDir(layout.MainWorktreeRoot, layout.CommonDir) {
			return layout, fmt.Errorf("linked worktree root %q does not own common Git directory %q", layout.MainWorktreeRoot, layout.CommonDir)
		}
		layout.Kind = GitLayoutLinkedWorktree
		return layout, nil
	case paths.GitDirLayoutBareWorktree:
		if err := validateLinkedLayout(layout, hasCommonDir); err != nil {
			return layout, err
		}
		if filepath.Base(layout.CommonDir) != ".bare" {
			return layout, fmt.Errorf("bare-worktree common directory %q is not named .bare", layout.CommonDir)
		}
		layout.MainWorktreeRoot = filepath.Dir(layout.CommonDir)
		if !rootOwnsGitDir(layout.MainWorktreeRoot, layout.CommonDir) {
			return layout, fmt.Errorf("bare-worktree container %q does not own common Git directory %q", layout.MainWorktreeRoot, layout.CommonDir)
		}
		layout.Kind = GitLayoutBareWorktree
		return layout, nil
	case paths.GitDirLayoutLinkedSubmodule:
		if err := validateLinkedLayout(layout, hasCommonDir); err != nil {
			return layout, err
		}
		layout.Kind = GitLayoutLinkedSubmodule
		return layout, nil
	}
	return layout, fmt.Errorf("unrecognized Git layout marker %d", parsed.Layout)
}

func commonGitDirIsBare(commonDir string) (bool, error) {
	data, err := readGitConfigFile(filepath.Join(commonDir, "config"))
	if err != nil {
		return false, fmt.Errorf("read common Git config: %w", err)
	}
	config, err := gitconfig.ReadConfig(bytes.NewReader(data))
	if err != nil {
		return false, fmt.Errorf("parse common Git config: %w", err)
	}
	return config.Core.IsBare, nil
}

func validateLinkedLayout(layout GitLayout, hasCommonDir bool) error {
	if !hasCommonDir {
		return errors.New("linked worktree has no commondir file")
	}
	wantGitDir := filepath.Join(layout.CommonDir, "worktrees", filepath.FromSlash(layout.WorktreeID))
	if layout.WorktreeID == "" || filepath.Clean(layout.GitDir) != filepath.Clean(wantGitDir) {
		return fmt.Errorf("gitdir %q contradicts common directory %q and worktree ID %q", layout.GitDir, layout.CommonDir, layout.WorktreeID)
	}

	data, err := readGitMetadataFile(filepath.Join(layout.GitDir, "gitdir"))
	if err != nil {
		return fmt.Errorf("read worktree registration: %w", err)
	}
	registeredDotGit := stringPath(data, layout.GitDir)
	if filepath.Base(registeredDotGit) != gitDir {
		return fmt.Errorf("worktree registration %q does not point to a .git entry", registeredDotGit)
	}
	registeredRoot, err := canonicalExistingPath(filepath.Dir(registeredDotGit))
	if err != nil {
		return fmt.Errorf("resolve worktree registration root: %w", err)
	}
	if registeredRoot != layout.WorktreeRoot {
		return fmt.Errorf("worktree registration points to %q instead of %q", registeredRoot, layout.WorktreeRoot)
	}
	return nil
}

func stringPath(data []byte, relativeTo string) string {
	path := filepath.Clean(strings.TrimSpace(string(data)))
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(relativeTo, path))
}

func rootOwnsGitDir(root, commonDir string) bool {
	resolved, err := resolveDotGitPath(root)
	if err != nil {
		return false
	}
	canonical, err := canonicalExistingPath(resolved)
	return err == nil && canonical == commonDir
}

func canonicalExistingPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("evaluate path symlinks: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func isGitCommonDir(path string) bool {
	head, headErr := os.Stat(filepath.Join(path, "HEAD"))
	objects, objectsErr := os.Stat(filepath.Join(path, "objects"))
	return headErr == nil && !head.IsDir() && objectsErr == nil && objects.IsDir()
}

func hasRegisteredWorktrees(commonDir string) bool {
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
