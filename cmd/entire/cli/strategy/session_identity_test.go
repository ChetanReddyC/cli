package strategy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/internal/proctree"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureStderrWriter redirects the strategy package's user-facing stderr into
// a buffer for the duration of the test.
func captureStderrWriter(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldWriter := stderrWriter
	stderrWriter = &buf
	t.Cleanup(func() { stderrWriter = oldWriter })
	return &buf
}

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

// selfAncestorRef returns a ProcessRef for this test process's parent — a
// live process guaranteed to appear in the ancestry of any process this test
// (or code it calls in-process) walks from.
func selfAncestorRef(t *testing.T) proctree.ProcessRef {
	t.Helper()
	ref, err := proctree.Ref(os.Getppid())
	require.NoError(t, err)
	return ref
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
		anc := selfAncestorRef(t)
		saveIdentitySession(t, "sess-agent", func(st *SessionState) {
			st.AgentAncestry = []proctree.ProcessRef{anc}
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
			st.AgentAncestry = nil
		})

		s := NewManualCommitStrategy()
		assert.Nil(t, s.findSessionByCommitAncestry(ctx, mustListStates(ctx, t, s)))
	})

	t.Run("dead process refs cannot match a recycled PID", func(t *testing.T) {
		identityTestRepo(t)
		saveIdentitySession(t, "sess-stale", func(st *SessionState) {
			// Same PID as a live ancestor, wrong start time: a recycled PID.
			ref := selfAncestorRef(t)
			ref.StartTime++
			st.AgentAncestry = []proctree.ProcessRef{ref}
		})

		s := NewManualCommitStrategy()
		assert.Nil(t, s.findSessionByCommitAncestry(ctx, mustListStates(ctx, t, s)))
	})

	t.Run("imported sessions never match", func(t *testing.T) {
		identityTestRepo(t)
		anc := selfAncestorRef(t)
		saveIdentitySession(t, "sess-imported", func(st *SessionState) {
			st.AgentAncestry = []proctree.ProcessRef{anc}
			st.Kind = session.KindImported
		})

		s := NewManualCommitStrategy()
		assert.Nil(t, s.findSessionByCommitAncestry(ctx, mustListStates(ctx, t, s)))
	})

	t.Run("same agent recorded by two sessions: latest interaction wins", func(t *testing.T) {
		// A resumed agent process produces a new session ID with the same
		// ancestry; the commit belongs to the one currently interacting.
		identityTestRepo(t)
		anc := selfAncestorRef(t)
		old := time.Now().Add(-2 * time.Hour)
		saveIdentitySession(t, "sess-old", func(st *SessionState) {
			st.AgentAncestry = []proctree.ProcessRef{anc}
			st.LastInteractionTime = &old
		})
		saveIdentitySession(t, "sess-new", func(st *SessionState) {
			st.AgentAncestry = []proctree.ProcessRef{anc}
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
	anc := selfAncestorRef(t)
	saveIdentitySession(t, "sess-agent-elsewhere", func(st *SessionState) {
		st.AgentAncestry = []proctree.ProcessRef{anc}
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
		anc := selfAncestorRef(t)
		saveIdentitySession(t, "sess-live-c", func(st *SessionState) {
			st.WorktreePath = wtA
			st.AgentAncestry = []proctree.ProcessRef{anc}
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

// Regression (Cursor bugbot on PR #2013): recording the raw ancestor chain
// captured the user's shell and terminal above the agent, so a human commit
// typed in the same terminal identity-matched the agent's session. Shell-like
// processes must be skipped (agents interpose `sh -c` below themselves) and
// everything above the first non-shell ancestor must stay unrecorded.
func TestIsShellLikeExe(t *testing.T) {
	t.Parallel()
	shellLike := []string{"sh", "bash", "zsh", "fish", "-fish", "dash", "tmux", "cmd.exe", "powershell.exe", "pwsh", "/bin/sh", "-bash",
		// tmux's server renames itself via prctl; a comm of "tmux: server"
		// must still classify as tmux or every pane shares a recordable ref.
		"tmux: server",
		// An unresolvable name is not evidence — treat as shell (skip), or an
		// unidentifiable process gets recorded as "the agent" even when it is
		// the very shell the filter exists to exclude.
		""}
	for _, exe := range shellLike {
		assert.True(t, isShellLikeExe(exe), "%q hosts arbitrary children; matching it proves plumbing, not authorship", exe)
	}
	notShell := []string{"claude", "node", "codex", "gemini", "entire", "go", "gotestsum", "strategy.test", "Cursor.exe"}
	for _, exe := range notShell {
		assert.False(t, isShellLikeExe(exe), "%q must remain recordable as an agent process", exe)
	}
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
