package strategy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/proclive"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// identityTestRepo initializes an isolated repo and chdirs into it.
func identityTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	return dir
}

// selfAncestorOwner returns the proclive.Identity of this test process's
// parent — a live process guaranteed to appear in the ancestry of any code
// this test calls in-process, standing in for the agent process that
// captureSessionOwner records as SessionState.Owner.
func selfAncestorOwner(t *testing.T) *proclive.Identity {
	t.Helper()
	id, ok := proclive.IdentityOf(os.Getppid())
	require.True(t, ok, "IdentityOf(parent) must resolve on supported platforms")
	return &id
}

func saveIdentitySession(t *testing.T, id string, mutate func(*SessionState)) {
	t.Helper()
	now := time.Now()
	state := &SessionState{
		SessionID:           id,
		BaseCommit:          "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		StartedAt:           now,
		LastInteractionTime: &now,
		Phase:               session.PhaseActive,
	}
	if mutate != nil {
		mutate(state)
	}
	require.NoError(t, SaveSessionState(context.Background(), state))
}

// mustListStates loads all session states for the states-accepting matchers.
func mustListStates(ctx context.Context, t *testing.T, s *ManualCommitStrategy) []*SessionState {
	t.Helper()
	states, err := s.listAllSessionStates(ctx)
	require.NoError(t, err)
	return states
}

// Regression: commit-to-session linking guessed by worktree path, so an agent
// committing in a sibling worktree lost linkage (or, with a poisoned state
// dir, linked to the wrong session — the sessC dangling-trailer incident).
// Identity matching attributes the commit to the session whose recorded agent
// process is an ancestor of the committing process, regardless of worktree.
//
// Not parallel: uses t.Chdir()
func TestFindSessionByCommitAncestry(t *testing.T) {
	ctx := context.Background()

	t.Run("matches the session whose agent is in the commit ancestry", func(t *testing.T) {
		identityTestRepo(t)
		anc := selfAncestorOwner(t)
		saveIdentitySession(t, "sess-agent", func(st *SessionState) {
			st.Owner = anc
			st.WorktreePath = "/somewhere/else/entirely"
		})

		s := NewManualCommitStrategy()
		got := s.findSessionByCommitAncestry(ctx, mustListStates(ctx, t, s))
		require.NotNil(t, got)
		assert.Equal(t, "sess-agent", got.SessionID, "identity match must ignore worktree paths")
	})

	t.Run("no recorded ancestry matches nothing", func(t *testing.T) {
		identityTestRepo(t)
		saveIdentitySession(t, "sess-plain", func(st *SessionState) {
			st.Owner = nil
		})

		s := NewManualCommitStrategy()
		assert.Nil(t, s.findSessionByCommitAncestry(ctx, mustListStates(ctx, t, s)))
	})

	t.Run("dead process refs cannot match a recycled PID", func(t *testing.T) {
		identityTestRepo(t)
		saveIdentitySession(t, "sess-stale", func(st *SessionState) {
			// Same PID as a live ancestor, wrong start fingerprint: a
			// recycled PID.
			owner := *selfAncestorOwner(t)
			owner.Start += "-recycled"
			st.Owner = &owner
		})

		s := NewManualCommitStrategy()
		assert.Nil(t, s.findSessionByCommitAncestry(ctx, mustListStates(ctx, t, s)))
	})

	t.Run("imported sessions never match", func(t *testing.T) {
		identityTestRepo(t)
		anc := selfAncestorOwner(t)
		saveIdentitySession(t, "sess-imported", func(st *SessionState) {
			st.Owner = anc
			st.Kind = session.KindImported
		})

		s := NewManualCommitStrategy()
		assert.Nil(t, s.findSessionByCommitAncestry(ctx, mustListStates(ctx, t, s)))
	})

	t.Run("nested agent beats outer agent: nearest ancestor wins over recency", func(t *testing.T) {
		// Two real ancestors at different depths stand in for a nested agent
		// (nearer the commit) and the outer agent that spawned it. Depth must
		// decide the winner; interaction recency is only a tiebreak within one
		// depth — so the nearer, less recently interacting session wins.
		identityTestRepo(t)
		ancestry, ok := proclive.CurrentAncestry()
		require.True(t, ok)
		chain := ancestry.Chain()
		if len(chain) < 2 {
			t.Skip("test process has fewer than two introspectable ancestors")
		}
		nested, outer := chain[0], chain[1]
		old := time.Now().Add(-2 * time.Hour)
		saveIdentitySession(t, "sess-nested", func(st *SessionState) {
			st.Owner = &nested
			st.LastInteractionTime = &old
		})
		saveIdentitySession(t, "sess-outer", func(st *SessionState) {
			st.Owner = &outer
		})

		s := NewManualCommitStrategy()
		got := s.findSessionByCommitAncestry(ctx, mustListStates(ctx, t, s))
		require.NotNil(t, got)
		assert.Equal(t, "sess-nested", got.SessionID,
			"the session whose agent is closest to the commit is its author, regardless of which interacted last")
	})

	t.Run("same agent recorded by two sessions: latest interaction wins", func(t *testing.T) {
		// A resumed agent process produces a new session ID with the same
		// ancestry; the commit belongs to the one currently interacting.
		identityTestRepo(t)
		anc := selfAncestorOwner(t)
		old := time.Now().Add(-2 * time.Hour)
		saveIdentitySession(t, "sess-old", func(st *SessionState) {
			st.Owner = anc
			st.LastInteractionTime = &old
		})
		saveIdentitySession(t, "sess-new", func(st *SessionState) {
			st.Owner = anc
		})

		s := NewManualCommitStrategy()
		got := s.findSessionByCommitAncestry(ctx, mustListStates(ctx, t, s))
		require.NotNil(t, got)
		assert.Equal(t, "sess-new", got.SessionID)
	})
}

// Not parallel: uses t.Chdir()
func TestFindSessionsForCommitLinking_IdentityAddsGuestSession(t *testing.T) {
	ctx := context.Background()
	dir := identityTestRepo(t)

	// The exact-worktree session path matching finds (its pending content is
	// part of this worktree's commit and must stay in the set)...
	saveIdentitySession(t, "sess-here", func(st *SessionState) {
		st.WorktreePath = dir
	})
	// ...and the agent session bound elsewhere whose process made the commit
	// — invisible to path matching (exact matches exist, so the sibling
	// fallback never fires), which is exactly how sibling-worktree agent
	// commits lost their linkage.
	anc := selfAncestorOwner(t)
	saveIdentitySession(t, "sess-agent-elsewhere", func(st *SessionState) {
		st.Owner = anc
		st.WorktreePath = "/somewhere/else/entirely"
	})

	s := NewManualCommitStrategy()
	got, err := s.findSessionsForCommitLinking(ctx, dir)
	require.NoError(t, err)
	require.Len(t, got, 2, "worktree set plus the identity-matched guest")
	ids := []string{got[0].SessionID, got[1].SessionID}
	assert.Contains(t, ids, "sess-here")
	assert.Contains(t, ids, "sess-agent-elsewhere")
}

// addSiblingWorktree creates a real git worktree of dir so fallback matching
// (which verifies a shared git common dir) can see sessions recorded there.
func addSiblingWorktree(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-"+name)
	cmd := exec.CommandContext(context.Background(), "git", "-C", dir, "worktree", "add", "-b", name, path)
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	return path
}

// Regression: leaked imported fixture states (sessC) were eligible for
// commit linking and hijacked a commit's trailer. Imported sessions are
// historical records — a fresh commit must never link to one.
//
// Not parallel: uses t.Chdir()
func TestFindSessionsForWorktree_ImportedSessionsNeverLink(t *testing.T) {
	dir := identityTestRepo(t)
	saveIdentitySession(t, "sess-imported-here", func(st *SessionState) {
		st.WorktreePath = dir
		st.Kind = session.KindImported
	})

	s := NewManualCommitStrategy()
	got, err := s.findSessionsForWorktree(context.Background(), dir)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// Regression: a commit in a worktree with no sessions of its own, while
// candidate sessions existed in several other worktrees, declined to link
// even when only ONE of those sessions had interacted recently — the
// days-idle stragglers vetoed the obviously-live one. Liveness filters the
// ambiguity; two genuinely live worktrees still decline (never guess), now
// with a stderr hint instead of only a log line.
//
// Not parallel: uses t.Chdir()
func TestFindSessionsForWorktree_AmbiguityResolvedByLiveness(t *testing.T) {
	ctx := context.Background()

	t.Run("single live worktree wins over stale ones", func(t *testing.T) {
		dir := identityTestRepo(t)
		wtStale := addSiblingWorktree(t, dir, "stale")
		wtLive := addSiblingWorktree(t, dir, "live")
		stale := time.Now().Add(-48 * time.Hour)
		saveIdentitySession(t, "sess-stale", func(st *SessionState) {
			st.WorktreePath = wtStale
			st.LastInteractionTime = &stale
		})
		saveIdentitySession(t, "sess-live", func(st *SessionState) {
			st.WorktreePath = wtLive
		})

		s := NewManualCommitStrategy()
		got, err := s.findSessionsForWorktree(ctx, dir)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "sess-live", got[0].SessionID)
	})

	t.Run("all candidates stale still declines", func(t *testing.T) {
		// The liveness filter must not turn "everything is days idle" into a
		// guess: with no recently-interacting session, spanning worktrees
		// stays ambiguous.
		dir := identityTestRepo(t)
		wtA := addSiblingWorktree(t, dir, "stale-a")
		wtB := addSiblingWorktree(t, dir, "stale-b")
		stale := time.Now().Add(-48 * time.Hour)
		saveIdentitySession(t, "sess-stale-a", func(st *SessionState) {
			st.WorktreePath = wtA
			st.LastInteractionTime = &stale
		})
		saveIdentitySession(t, "sess-stale-b", func(st *SessionState) {
			st.WorktreePath = wtB
			st.LastInteractionTime = &stale
		})

		s := NewManualCommitStrategy()
		got, err := s.findSessionsForWorktree(ctx, dir)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("two live worktrees decline, audibly — but only from the commit-linking path", func(t *testing.T) {
		dir := identityTestRepo(t)
		wtA := addSiblingWorktree(t, dir, "live-a")
		wtB := addSiblingWorktree(t, dir, "live-b")
		saveIdentitySession(t, "sess-live-a", func(st *SessionState) { st.WorktreePath = wtA })
		saveIdentitySession(t, "sess-live-b", func(st *SessionState) { st.WorktreePath = wtB })
		buf := captureStderrWriter(t)

		s := NewManualCommitStrategy()
		// The plain worktree matcher (amend/post-rewrite callers) declines
		// silently: an adopt hint on a history edit is noise.
		got, err := s.findSessionsForWorktree(ctx, dir)
		require.NoError(t, err)
		assert.Empty(t, got, "two live sessions in different worktrees is a genuine ambiguity — never guess")
		assert.Empty(t, buf.String(), "amend/post-rewrite paths must not print the adopt hint")

		// The commit-linking path announces the decline with the remedy —
		// only because identity matching could not rescue the commit either.
		got, err = s.findSessionsForCommitLinking(ctx, dir)
		require.NoError(t, err)
		assert.Empty(t, got)
		assert.Contains(t, buf.String(), "session adopt", "an unlinked commit must say so on stderr, not hide in a log file")
	})

	t.Run("identity rescue suppresses the decline hint", func(t *testing.T) {
		// Regression (review finding on PR #2013): the hint used to print
		// from inside the worktree matcher, BEFORE the identity union ran —
		// so the PR's headline scenario (agent commit in a sibling worktree
		// rescued by identity) printed a false "none was linked" on a commit
		// that got a trailer moments later.
		dir := identityTestRepo(t)
		wtA := addSiblingWorktree(t, dir, "live-c")
		wtB := addSiblingWorktree(t, dir, "live-d")
		anc := selfAncestorOwner(t)
		saveIdentitySession(t, "sess-live-c", func(st *SessionState) {
			st.WorktreePath = wtA
			st.Owner = anc
		})
		saveIdentitySession(t, "sess-live-d", func(st *SessionState) { st.WorktreePath = wtB })
		buf := captureStderrWriter(t)

		s := NewManualCommitStrategy()
		got, err := s.findSessionsForCommitLinking(ctx, dir)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, "sess-live-c", got[0].SessionID, "identity must rescue the ambiguous commit")
		assert.Empty(t, buf.String(), "a rescued commit must not tell the user nothing was linked")
	})
}

// Guest-linked sessions (identity-matched from a worktree other than their
// home) must never have worktree-coupled state advanced by the foreign
// commit: BaseCommit keys the shadow branch, and rewriting it from another
// worktree orphans that branch and breaks the session's next home commit.
// The inverse matters just as much — a session committed in its OWN worktree
// must keep advancing, or BaseCommit tracking silently freezes for everyone.
//
// Not parallel: uses t.Chdir()
func TestUpdateBaseCommitIfChanged_GuestWorktreeGating(t *testing.T) {
	ctx := context.Background()
	dir := identityTestRepo(t)
	s := NewManualCommitStrategy()

	home := &SessionState{
		SessionID:    "sess-home-gate",
		BaseCommit:   "1111111111111111111111111111111111111111",
		WorktreePath: dir,
		Phase:        session.PhaseActive,
	}
	s.updateBaseCommitIfChanged(ctx, home, "2222222222222222222222222222222222222222", dir)
	assert.Equal(t, "2222222222222222222222222222222222222222", home.BaseCommit,
		"a session in its home worktree must keep advancing BaseCommit")

	guest := &SessionState{
		SessionID:    "sess-guest-gate",
		BaseCommit:   "1111111111111111111111111111111111111111",
		WorktreePath: "/somewhere/else/entirely",
		Phase:        session.PhaseActive,
	}
	s.updateBaseCommitIfChanged(ctx, guest, "2222222222222222222222222222222222222222", dir)
	assert.Equal(t, "1111111111111111111111111111111111111111", guest.BaseCommit,
		"a guest-linked session's BaseCommit tracks its home worktree, not this commit")
}

// Not parallel: uses t.Chdir()
func TestFindSessionsForCommitLinking_FallsBackToWorktree(t *testing.T) {
	ctx := context.Background()
	dir := identityTestRepo(t)

	saveIdentitySession(t, "sess-here-fallback", func(st *SessionState) {
		st.WorktreePath = dir
	})

	s := NewManualCommitStrategy()
	got, err := s.findSessionsForCommitLinking(ctx, dir)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "sess-here-fallback", got[0].SessionID,
		"human commits (no agent ancestry) keep worktree matching")
}
