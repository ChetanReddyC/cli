package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GitDirLayout identifies the structural marker in a checkout's gitdir path.
type GitDirLayout uint8

const (
	GitDirLayoutUnrecognized GitDirLayout = iota
	GitDirLayoutLinkedWorktree
	GitDirLayoutBareWorktree
	GitDirLayoutSubmodule
	GitDirLayoutLinkedSubmodule
)

// WorktreeGitDir is the parsed structural information from a checkout's
// resolved gitdir path.
type WorktreeGitDir struct {
	Layout     GitDirLayout
	WorktreeID string
}

// GetWorktreeID returns the internal git worktree identifier for the given path.
// For the main worktree (where .git is a directory), returns empty string.
// For linked worktrees (where .git is a file), extracts the name from
// .git/worktrees/<name>/ path. This name is stable across `git worktree move`.
func GetWorktreeID(worktreePath string) (string, error) {
	gitPath := filepath.Join(worktreePath, ".git")

	info, err := os.Stat(gitPath)
	if err != nil {
		return "", fmt.Errorf("failed to stat .git: %w", err)
	}

	// Main worktree has .git as a directory
	if info.IsDir() {
		return "", nil
	}

	// Linked worktree has .git as a file with content: "gitdir: /path/to/.git/worktrees/<name>"
	content, err := os.ReadFile(gitPath) //nolint:gosec // gitPath is constructed from worktreePath + ".git"
	if err != nil {
		return "", fmt.Errorf("failed to read .git file: %w", err)
	}

	line := strings.TrimSpace(string(content))
	if !strings.HasPrefix(line, "gitdir: ") {
		return "", fmt.Errorf("invalid .git file format: %s", line)
	}

	gitdir := strings.TrimPrefix(line, "gitdir: ")
	if worktreeID, found := parseWorktreeID(gitdir); found {
		return worktreeID, nil
	}

	return "", fmt.Errorf("unexpected gitdir format (no worktrees): %s", gitdir)
}

func parseWorktreeID(gitdir string) (string, bool) {
	parsed := ParseWorktreeGitDir(gitdir)
	if parsed.Layout == GitDirLayoutUnrecognized {
		return "", false
	}
	return parsed.WorktreeID, true
}

// ParseWorktreeGitDir classifies the conventional Git path markers used for
// linked worktrees, bare-worktree containers, and submodules.
func ParseWorktreeGitDir(gitdir string) WorktreeGitDir {
	gitdir = strings.TrimSuffix(strings.ReplaceAll(gitdir, "\\", "/"), "/")

	// Submodule gitdirs live under .git/modules/<path>. If that submodule
	// repository has its own linked worktree, the gitdir ends with
	// .git/modules/<path>/worktrees/<id>. A /worktrees/ segment before the
	// final /modules/ belongs to the superproject's worktree, not the submodule.
	if modulesIndex := strings.LastIndex(gitdir, "/modules/"); modulesIndex >= 0 {
		afterModules := gitdir[modulesIndex+len("/modules/"):]
		if _, worktreeID, found := strings.Cut(afterModules, "/worktrees/"); found {
			return WorktreeGitDir{
				Layout:     GitDirLayoutLinkedSubmodule,
				WorktreeID: strings.TrimSuffix(worktreeID, "/"),
			}
		}
		return WorktreeGitDir{Layout: GitDirLayoutSubmodule}
	}

	// Extract worktree name from path like /repo/.git/worktrees/<name>
	// or /repo/.bare/worktrees/<name> (bare repo + worktree layout).
	// The path after the marker is the worktree identifier.
	if _, worktreeID, found := strings.Cut(gitdir, ".git/worktrees/"); found {
		return WorktreeGitDir{
			Layout:     GitDirLayoutLinkedWorktree,
			WorktreeID: strings.TrimSuffix(worktreeID, "/"),
		}
	}
	if _, worktreeID, found := strings.Cut(gitdir, ".bare/worktrees/"); found {
		return WorktreeGitDir{
			Layout:     GitDirLayoutBareWorktree,
			WorktreeID: strings.TrimSuffix(worktreeID, "/"),
		}
	}

	return WorktreeGitDir{Layout: GitDirLayoutUnrecognized}
}
