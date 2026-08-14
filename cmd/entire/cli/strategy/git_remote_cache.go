package strategy

import (
	"context"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

// The checkpoint sync remote is elected from scratch on every call — a
// documented tradeoff (see CheckpointReadRemotes), and every election shells out
// to git to answer two questions about .git/config: which remotes exist, and
// does remote X exist. Those answers are identical for every election within one
// command against one repository, and there are several: `entire checkpoint list`
// resolves the chain four times (metadata-disconnection warning, the branch
// listing itself, the git-refs store's remote discovery, and the
// imported-rewind-point pass), costing 9 git subprocesses where the same command
// on the pre-election code spent 3.
//
// This caches only those two .git/config reads — never the election result.
// Settings and the captured-election file (#1991) stay uncached, so a write
// followed by a re-resolve still observes the new value; only "which git remotes
// exist" is memoized.
//
// Context-scoped and opt-in rather than a package global: an uninstrumented
// context behaves exactly as before, so tests keep the uncached path and cannot
// leak one temp repo's remote list into another. main() installs it on the root
// context; `entire mcp` narrows that to one window per request (see
// WithGitRemoteCache).

type gitRemoteCacheKey struct{}

// gitRemoteCache memoizes .git/config remote reads, partitioned by repository.
//
// The partition is load-bearing, not tidiness: `entire dispatch --repos a,b`
// walks several repositories in ONE process, scoping each one's election with
// settings.WithWorktreeRoot so the git calls run in that repo (see
// dispatch/mode_local.go, and c04a2e312 "honor read candidates per repository",
// which fixed exactly this). A cache keyed only by remote name would answer
// repo B's election from repo A's remote list and silently re-break that fix.
//
// Guarded by a mutex because a single command may resolve the chain from several
// goroutines (checkpoint hydration fans out).
type gitRemoteCache struct {
	mu sync.Mutex
	// byDir maps the repository root to its memoized answers.
	byDir map[string]*remoteSnapshot
}

// remoteSnapshot holds one repository's memoized .git/config answers.
type remoteSnapshot struct {
	// ordered is configuredRemotesInConfigOrder's result; orderedSet
	// distinguishes "not yet read" from a legitimately empty remote list.
	ordered    []string
	orderedSet bool
	// member holds isConfiguredRemote answers per remote name. Kept separate
	// from ordered because the two ask git different questions: ordered lists
	// only remotes carrying a url key in local config, while isConfiguredRemote
	// runs `git remote get-url`, which also sees pushurl-only remotes and
	// inherited scopes. Deriving one from the other would change election
	// semantics, so each is memoized against its own git call.
	member map[string]bool
}

// WithGitRemoteCache returns a context whose .git/config remote reads are
// memoized for the lifetime of that context, reusing an already-installed cache
// if there is one. Never span a git-remote mutation without calling
// InvalidateGitRemoteCache.
//
// main() installs this on the root context, which is the whole process. Answers
// are partitioned per repository (see gitRemoteCache), so a command
// walking several repositories stays correct. What process scope does NOT give is
// freshness over time: `entire mcp` serves an agent session from one context, so
// it installs a fresh window per request rather than pinning one snapshot for
// hours. A command that runs long enough for a remote to be added underneath it
// should do the same.
func WithGitRemoteCache(ctx context.Context) context.Context {
	if cacheFromContext(ctx) != nil {
		return ctx
	}
	return WithFreshGitRemoteCache(ctx)
}

// WithFreshGitRemoteCache installs a new cache even when the parent context
// already carries one, giving a long-lived process a per-unit-of-work window
// instead of one snapshot for its whole lifetime.
func WithFreshGitRemoteCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, gitRemoteCacheKey{}, &gitRemoteCache{byDir: map[string]*remoteSnapshot{}})
}

// InvalidateGitRemoteCache drops the memoized remote reads for every repository.
// Callers that add, rename, remove or re-point a git remote must call this, or
// later elections in the same command answer from the pre-mutation config. A
// caller performing several mutations needs one call after the last of them.
func InvalidateGitRemoteCache(ctx context.Context) {
	c := cacheFromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byDir = map[string]*remoteSnapshot{}
}

func cacheFromContext(ctx context.Context) *gitRemoteCache {
	c, ok := ctx.Value(gitRemoteCacheKey{}).(*gitRemoteCache)
	if !ok {
		return nil
	}
	return c
}

// gitReadRepo identifies the repository the memoized git reads resolve against,
// and so the cache partition: the context's scoped worktree root when there is
// one, else the repo containing the process working directory that `git` would
// inherit. Returns false when neither can be established, which callers read as
// "do not cache" — an unidentifiable repository must not share another's answers.
//
// The repo root rather than the raw cwd: `git remote get-url` answers the same
// from any subdirectory of a repository, so two calls from different
// subdirectories should share one answer. paths.WorktreeRoot is memoized per cwd,
// so this stays far cheaper than the subprocess it is protecting.
func gitReadRepo(ctx context.Context) (string, bool) {
	if root, ok := settings.WorktreeRoot(ctx); ok && root != "" {
		return root, true
	}
	root, err := paths.WorktreeRoot(ctx)
	if err != nil || root == "" {
		return "", false
	}
	return root, true
}

// snapshotFor returns the per-repository snapshot, or nil when this call must not
// be cached. Callers hold c.mu.
func (c *gitRemoteCache) snapshotFor(ctx context.Context) *remoteSnapshot {
	repoRoot, ok := gitReadRepo(ctx)
	if !ok {
		return nil
	}
	snap := c.byDir[repoRoot]
	if snap == nil {
		snap = &remoteSnapshot{member: map[string]bool{}}
		c.byDir[repoRoot] = snap
	}
	return snap
}

// cachedRemotesInConfigOrder returns the memoized remote list for this call's
// repository, computing it via read on a miss. Without a cache installed, or when
// the repository cannot be established, it just calls read.
func cachedRemotesInConfigOrder(ctx context.Context, read func(context.Context) []string) []string {
	c := cacheFromContext(ctx)
	if c == nil {
		return read(ctx)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.snapshotFor(ctx)
	if snap == nil {
		return read(ctx)
	}
	if !snap.orderedSet {
		snap.ordered, snap.orderedSet = read(ctx), true
	}
	return snap.ordered
}

// cachedIsConfiguredRemote returns the memoized answer for name in this call's
// repository, computing it via probe on a miss. Without a cache installed, or when
// the repository cannot be established, it just calls probe.
func cachedIsConfiguredRemote(ctx context.Context, name string, probe func() bool) bool {
	c := cacheFromContext(ctx)
	if c == nil {
		return probe()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.snapshotFor(ctx)
	if snap == nil {
		return probe()
	}
	if got, ok := snap.member[name]; ok {
		return got
	}
	got := probe()
	snap.member[name] = got
	return got
}
