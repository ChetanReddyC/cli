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
		got := s.findSessionByCommitAncestry(ctx)
		require.NotNil(t, got)
		assert.Equal(t, "sess-agent", got.SessionID, "identity match must ignore worktree paths")
	})

	t.Run("no recorded ancestry matches nothing", func(t *testing.T) {
		identityTestRepo(t)
		saveIdentitySession(t, "sess-plain", func(st *SessionState) {
			st.AgentAncestry = nil
		})

		s := NewManualCommitStrategy()
		assert.Nil(t, s.findSessionByCommitAncestry(ctx))
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
		assert.Nil(t, s.findSessionByCommitAncestry(ctx))
	})

	t.Run("imported sessions never match", func(t *testing.T) {
		identityTestRepo(t)
		anc := selfAncestorRef(t)
		saveIdentitySession(t, "sess-imported", func(st *SessionState) {
			st.AgentAncestry = []proctree.ProcessRef{anc}
			st.Kind = session.KindImported
		})

		s := NewManualCommitStrategy()
		assert.Nil(t, s.findSessionByCommitAncestry(ctx))
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
		got := s.findSessionByCommitAncestry(ctx)
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

	t.Run("two live worktrees still decline, audibly", func(t *testing.T) {
		dir := identityTestRepo(t)
		wtA := addSiblingWorktree(t, dir, "live-a")
		wtB := addSiblingWorktree(t, dir, "live-b")
		saveIdentitySession(t, "sess-live-a", func(st *SessionState) { st.WorktreePath = wtA })
		saveIdentitySession(t, "sess-live-b", func(st *SessionState) { st.WorktreePath = wtB })
		buf := captureStderrWriter(t)

		s := NewManualCommitStrategy()
		got, err := s.findSessionsForWorktree(ctx, dir)
		require.NoError(t, err)
		assert.Empty(t, got, "two live sessions in different worktrees is a genuine ambiguity — never guess")
		assert.Contains(t, buf.String(), "session adopt", "the decline must tell the user the remedy, not hide in a log file")
	})
}

// Regression (Cursor bugbot on PR #2013): recording the raw ancestor chain
// captured the user's shell and terminal above the agent, so a human commit
// typed in the same terminal identity-matched the agent's session. Shell-like
// processes must be skipped (agents interpose `sh -c` below themselves) and
// everything above the first non-shell ancestor must stay unrecorded.
func TestIsShellLikeExe(t *testing.T) {
	t.Parallel()
	shellLike := []string{"sh", "bash", "zsh", "fish", "-fish", "dash", "tmux", "cmd.exe", "powershell.exe", "pwsh", "/bin/sh", "-bash"}
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
