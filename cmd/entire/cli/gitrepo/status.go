package gitrepo

import (
	"context"

	"github.com/go-git/go-git/v6"
)

// Status is the single entry point for reading go-git worktree status; the
// forbidigo rule in .golangci.yaml keeps callers off worktree.Status directly.
//
// Worktree.Status() walks the worktree, so its cost scales with working-set size
// rather than with the size of the change being inspected — it is the most
// expensive git read on the hook paths, around 120ms on a 20k-file tree. Read it
// once per hook and pass the result down rather than calling this repeatedly.
func Status(ctx context.Context, repo *git.Repository) (git.Status, error) {
	_ = ctx // accepted for symmetry with the other gitrepo entry points

	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err //nolint:wrapcheck // callers add their own context
	}

	return worktree.Status() //nolint:wrapcheck,forbidigo // the sanctioned call site
}
