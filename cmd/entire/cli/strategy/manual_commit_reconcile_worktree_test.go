package strategy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/stretchr/testify/require"
)

// TestReconcileWorktreePathForResumedTurn_RepointsAfterRelocation covers the
// #1890 relocation case: the recorded worktree path no longer resolves (the repo
// was renamed/moved), so a resumed turn must repoint WorktreePath at the current
// worktree, which shares the same git common dir as the session store.
func TestReconcileWorktreePathForResumedTurn_RepointsAfterRelocation(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	mainDir := setupSessionMatchRepo(t)
	gonePath := resolvedRemovedTempDir(t)

	state := &SessionState{SessionID: "relocated-session", WorktreePath: gonePath}

	t.Chdir(mainDir)
	clearSessionMatchCaches()

	reconcileWorktreePathForResumedTurn(ctx, state)

	require.Equal(t, mainDir, state.WorktreePath,
		"a stale recorded path should be repointed at the current worktree")
}

// TestReconcileWorktreePathForResumedTurn_LeavesValidSiblingUntouched proves the
// guard: when the recorded path is still a live worktree of this same repo (a
// concurrent sibling), reconciliation must not steal it, even though the current
// worktree differs.
func TestReconcileWorktreePathForResumedTurn_LeavesValidSiblingUntouched(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	mainDir := setupSessionMatchRepo(t)
	sibling := resolvedRemovedTempDir(t)
	createSessionMatchWorktree(t, mainDir, sibling, "sibling")
	t.Cleanup(func() { removeSessionMatchWorktree(mainDir, sibling) })

	state := &SessionState{SessionID: "sibling-session", WorktreePath: sibling}

	t.Chdir(mainDir)
	clearSessionMatchCaches()

	reconcileWorktreePathForResumedTurn(ctx, state)

	require.Equal(t, sibling, state.WorktreePath,
		"a recorded path that still resolves to this repo must be left untouched")
}

// TestReconcileWorktreePathForResumedTurn_PreservesWorktreeIDAcrossRelocation
// covers the reachable data-loss path: a session started in a linked worktree
// whose path is later gone (whole-repo relocation), resumed from the main
// worktree. Reconcile must repoint WorktreePath without changing WorktreeID —
// the shadow branch is keyed on WorktreeID, so rewriting it (main worktree ID is
// "") would orphan the session's prior checkpoints and lose rewind history.
func TestReconcileWorktreePathForResumedTurn_PreservesWorktreeIDAcrossRelocation(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	mainDir := setupSessionMatchRepo(t)
	linkedDir := filepath.Join(mainDir, ".worktrees", "feature")
	createSessionMatchWorktree(t, mainDir, linkedDir, "feature")

	linkedID, err := paths.GetWorktreeID(linkedDir)
	require.NoError(t, err)
	require.NotEmpty(t, linkedID, "linked worktree must have a non-empty WorktreeID")

	state := &SessionState{
		SessionID:    "relocated-linked-session",
		WorktreePath: linkedDir,
		WorktreeID:   linkedID,
	}

	// Remove the linked worktree so its recorded path no longer resolves to this
	// repo's git common dir — the trigger condition for reconciliation.
	removeSessionMatchWorktree(mainDir, linkedDir)

	t.Chdir(mainDir)
	clearSessionMatchCaches()

	reconcileWorktreePathForResumedTurn(ctx, state)

	require.Equal(t, mainDir, state.WorktreePath,
		"a stale recorded path should be repointed at the current worktree")
	require.Equal(t, linkedID, state.WorktreeID,
		"WorktreeID must be preserved so the session's existing shadow branch stays reachable")
}

// TestReconcileWorktreePathForResumedTurn_NoopWhenPathUnchanged covers the common
// path: a normal turn from the recorded worktree leaves the state alone.
func TestReconcileWorktreePathForResumedTurn_NoopWhenPathUnchanged(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	ctx := context.Background()

	mainDir := setupSessionMatchRepo(t)

	state := &SessionState{SessionID: "steady-session", WorktreePath: mainDir}

	t.Chdir(mainDir)
	clearSessionMatchCaches()

	reconcileWorktreePathForResumedTurn(ctx, state)

	require.Equal(t, mainDir, state.WorktreePath)
}
