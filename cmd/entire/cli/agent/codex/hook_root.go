package codex

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// HookDiscoveryState distinguishes a resolved Codex hook source from a layout
// whose behavior Entire cannot safely infer.
type HookDiscoveryState uint8

const (
	HookDiscoveryUnresolved HookDiscoveryState = iota
	HookDiscoveryResolved
)

// DiscoveredHooksPath identifies the read-only project hooks file Codex is
// expected to load. It is intentionally distinct from WorktreeHooksPath.
type DiscoveredHooksPath struct {
	path string
}

// Path returns the discovered hooks file's absolute path.
func (p DiscoveredHooksPath) Path() string {
	return p.path
}

// HookDiscovery describes the hooks file Codex is expected to discover. It is
// diagnostic-only and carries no write target, migration path, or lock path.
type HookDiscovery struct {
	State           HookDiscoveryState
	DiscoveredHooks DiscoveredHooksPath
	RepositoryWide  bool
	Diagnostic      error
	worktreeRoot    string
}

// ProjectLayerExists reports whether the current checkout has the local
// .codex directory Codex needs to construct its project config layer.
func (d HookDiscovery) ProjectLayerExists() bool {
	return d.worktreeRoot != "" && projectLayerExists(filepath.Join(d.worktreeRoot, ".codex"))
}

// UnresolvedHookDiscoveryError explains why Entire will not guess which hook
// file Codex loads for a Git layout.
type UnresolvedHookDiscoveryError struct {
	Reason string
}

func (e *UnresolvedHookDiscoveryError) Error() string {
	return "Codex hook discovery is unresolved: " + e.Reason
}

// HookInstallationSkipped marks this permanent safety refusal as non-fatal to
// setup for other selected agents.
func (e *UnresolvedHookDiscoveryError) HookInstallationSkipped() {}

// ResolveHookDiscovery performs read-only discovery of the hook file Codex is
// expected to load for the current checkout.
func ResolveHookDiscovery(ctx context.Context) HookDiscovery {
	layout, err := gitrepo.ResolveGitLayout(ctx)
	if err != nil {
		return unresolvedHookDiscoveryAt(layout.WorktreeRoot, "Git layout could not be resolved: "+err.Error())
	}
	return hookDiscoveryFromLayout(layout)
}

func resolveHookDiscovery(worktreeRoot string) HookDiscovery {
	layout, err := gitrepo.ResolveGitLayoutAt(worktreeRoot)
	if err != nil {
		return unresolvedHookDiscoveryAt(layout.WorktreeRoot, "Git layout could not be resolved: "+err.Error())
	}
	return hookDiscoveryFromLayout(layout)
}

func hookDiscoveryFromLayout(layout gitrepo.GitLayout) HookDiscovery {
	discovery := HookDiscovery{
		State:        HookDiscoveryResolved,
		worktreeRoot: layout.WorktreeRoot,
	}
	root := layout.WorktreeRoot

	switch layout.Kind {
	case gitrepo.GitLayoutNormal:
		discovery.RepositoryWide = layout.HasLinkedWorktrees
	case gitrepo.GitLayoutLinkedWorktree:
		root = layout.MainWorktreeRoot
		discovery.RepositoryWide = true
	case gitrepo.GitLayoutSubmodule, gitrepo.GitLayoutSeparateGitDir:
	case gitrepo.GitLayoutBareWorktree:
		return unresolvedHookDiscoveryAt(layout.WorktreeRoot, "Codex behavior for .bare/worktrees layouts is not pinned")
	case gitrepo.GitLayoutLinkedSubmodule:
		return unresolvedHookDiscoveryAt(layout.WorktreeRoot, "Codex behavior for linked submodules is not pinned")
	case gitrepo.GitLayoutUnresolved:
		return unresolvedHookDiscoveryAt(layout.WorktreeRoot, "Git layout classification is unresolved")
	default:
		return unresolvedHookDiscoveryAt(layout.WorktreeRoot, fmt.Sprintf("unknown Git layout kind %d", layout.Kind))
	}

	if root == "" {
		return unresolvedHookDiscoveryAt(layout.WorktreeRoot, "Git layout did not identify a hook root")
	}
	if isUserHookRoot(root) {
		return unresolvedHookDiscoveryAt(layout.WorktreeRoot, fmt.Sprintf("derived hook root %q is user-wide", root))
	}
	discovery.DiscoveredHooks = DiscoveredHooksPath{path: filepath.Join(root, ".codex", HooksFileName)}
	return discovery
}

func unresolvedHookDiscoveryAt(worktreeRoot, reason string) HookDiscovery {
	return HookDiscovery{
		State:        HookDiscoveryUnresolved,
		worktreeRoot: worktreeRoot,
		Diagnostic:   &UnresolvedHookDiscoveryError{Reason: reason},
	}
}

// WorktreeProjectLayerExists reports whether the current checkout has a local
// .codex project directory.
func WorktreeProjectLayerExists(ctx context.Context) bool {
	hooks, err := ResolveWorktreeHooksPath(ctx)
	return err == nil && projectLayerExists(filepath.Dir(hooks.Path()))
}

func projectLayerExists(projectDir string) bool {
	info, err := os.Stat(projectDir)
	return err == nil && info.IsDir()
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

// WorktreeHooksPath identifies the hooks file owned by the current checkout.
// It is intentionally distinct from DiscoveredHooksPath.
type WorktreeHooksPath struct {
	path         string
	worktreeRoot string
}

// Path returns the current checkout's hooks file path.
func (p WorktreeHooksPath) Path() string {
	return p.path
}

// ResolveWorktreeHooksPath resolves the hooks file owned by the current
// checkout, independently of the file Codex discovers.
func ResolveWorktreeHooksPath(ctx context.Context) (WorktreeHooksPath, error) {
	worktreeRoot, err := resolveWorktreeRoot(ctx)
	if err != nil {
		return WorktreeHooksPath{}, err
	}
	return resolveWorktreeHooksPath(worktreeRoot)
}

func resolveWorktreeHooksPath(worktreeRoot string) (WorktreeHooksPath, error) {
	canonicalRoot, err := canonicalPath(worktreeRoot)
	if err != nil {
		return WorktreeHooksPath{}, fmt.Errorf("resolve worktree root: %w", err)
	}
	return WorktreeHooksPath{
		path:         filepath.Join(canonicalRoot, ".codex", HooksFileName),
		worktreeRoot: canonicalRoot,
	}, nil
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
