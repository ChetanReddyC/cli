package gitrepo

import (
	"context"
	"sync"

	"github.com/go-git/go-git/v6"
)

// go-git's Worktree.Status() is expensive: it walks the whole worktree twice
// (once collecting .gitignore patterns, once diffing), and it does not prune
// ignored subtrees whose pattern was declared by an ancestor .gitignore. A
// single call costs seconds in a repo with a large ignored directory, so hooks
// that need the status more than once must not recompute it.
//
// Status is the single entry point for reading worktree status. When ctx
// carries a cache installed by WithStatusCache, the first call for a worktree
// computes the status and later calls with that ctx reuse it.

type statusCacheKey struct{}

type statusResult struct {
	status git.Status
	err    error
}

type statusCache struct {
	mu      sync.Mutex
	results map[string]statusResult
}

// WithStatusCache returns a context that memoizes Status results.
//
// Install it only across a window in which the worktree cannot change —
// otherwise later callers observe a stale status. A hook that runs before the
// agent acts (such as turn start) is such a window; a hook that runs after the
// agent has edited files is not.
func WithStatusCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, statusCacheKey{}, &statusCache{
		results: make(map[string]statusResult),
	})
}

// Status returns the worktree status for repo, reusing a cached result when ctx
// carries a cache from WithStatusCache and the same worktree was already read.
//
// The returned map is shared with other callers holding the same cached ctx, so
// callers must treat it as read-only.
func Status(ctx context.Context, repo *git.Repository) (git.Status, error) {
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}

	cache, ok := ctx.Value(statusCacheKey{}).(*statusCache)
	if !ok || cache == nil {
		return worktree.Status() //nolint:wrapcheck // callers add their own context
	}

	// Key on the worktree root rather than the repository pointer: callers on
	// the same hook path open the repository independently.
	root := worktree.Filesystem().Root()

	cache.mu.Lock()
	defer cache.mu.Unlock()

	if cached, ok := cache.results[root]; ok {
		return cached.status, cached.err
	}

	status, statusErr := worktree.Status()
	cache.results[root] = statusResult{status: status, err: statusErr}

	return status, statusErr //nolint:wrapcheck // callers add their own context
}
