package strategy

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/internal/proctree"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// agentAncestryDepth is how many ancestors the session-start hook records in
// SessionState.AgentAncestry. The chain above the hook is (shell that ran the
// hook) → agent → (terminal shell / supervisor); three levels reliably
// includes the agent process while staying below system-wide ancestors
// (terminal multiplexers, init) that would make unrelated processes look
// related.
const agentAncestryDepth = 3

// commitAncestryDepth is how far the commit hook walks its own ancestry when
// matching — deeper than the recording depth because agents interpose shells
// and wrappers between themselves and the git subprocess (hook ← git ← tool
// shell(s) ← agent ← ...).
const commitAncestryDepth = 12

// findSessionsForCommitLinking resolves which sessions a commit belongs to:
// the worktree-matched set (a commit captures the worktree's content, so
// every session with pending content here belongs in it — concurrent
// sessions interleave by design) PLUS the identity-matched session when the
// committing process's ancestry names one that worktree matching missed.
// The identity union is what makes an agent-made commit immune to worktree
// bookkeeping drift: an agent committing in a sibling worktree still links
// to its own session (guest-linked), where path matching alone found nothing
// or declined as ambiguous.
func (s *ManualCommitStrategy) findSessionsForCommitLinking(ctx context.Context, worktreePath string) ([]*SessionState, error) {
	sessions, err := s.findSessionsForWorktree(ctx, worktreePath)
	if err != nil {
		sessions = nil // identity below may still rescue linking
	}
	if guest := s.findSessionByCommitAncestry(ctx); guest != nil && !linkingSetContains(sessions, guest.SessionID) {
		sessions = append(sessions, guest)
		err = nil
	}
	return sessions, err
}

func linkingSetContains(states []*SessionState, id string) bool {
	for _, st := range states {
		if st.SessionID == id {
			return true
		}
	}
	return false
}

// findSessionByCommitAncestry attributes the running commit to the session
// whose recorded agent process is an ancestor of this hook process. Nearest
// ancestor wins: if two sessions recorded processes at different depths of
// this commit's ancestry (an agent vs. the terminal shell it runs in), the
// one closest to the commit is the author. Ties at the same depth — the same
// live process recorded by several sessions, e.g. a resumed agent — go to
// the most recently interacting session. Imported sessions are historical
// records and never match. Returns nil when nothing in the ancestry is a
// recorded agent (the caller falls back to worktree matching).
func (s *ManualCommitStrategy) findSessionByCommitAncestry(ctx context.Context) *SessionState {
	ancestors := proctree.Ancestors(commitAncestryDepth)
	if len(ancestors) == 0 {
		return nil
	}
	states, err := s.listAllSessionStates(ctx)
	if err != nil || len(states) == 0 {
		return nil
	}

	for _, anc := range ancestors { // nearest-first
		var best *SessionState
		for _, state := range states {
			if state.Kind.IsImported() || !refsContain(state.AgentAncestry, anc) {
				continue
			}
			if best == nil || interactedAfter(state, best) {
				best = state
			}
		}
		if best != nil {
			logging.Debug(logging.WithComponent(ctx, "checkpoint"),
				"commit attributed to session by process ancestry",
				slog.String("session_id", best.SessionID),
				slog.Int("ancestor_pid", anc.PID),
				slog.String("ancestor_exe", anc.Exe),
			)
			return best
		}
	}
	return nil
}

// isSessionHomeWorktree reports whether the current worktree is the one the
// session is recorded in. Worktree-coupled state (BaseCommit, shadow-branch
// realignment) may only be mutated from the session's home worktree; a
// guest-linked commit elsewhere condenses and links without moving it.
// Unresolvable worktree reads as home, preserving pre-identity behavior.
func isSessionHomeWorktree(ctx context.Context, state *SessionState) bool {
	worktreePath, err := paths.WorktreeRoot(ctx)
	if err != nil || worktreePath == "" {
		return true
	}
	return state.WorktreePath == "" || state.WorktreePath == worktreePath
}

func refsContain(refs []proctree.ProcessRef, target proctree.ProcessRef) bool {
	for _, r := range refs {
		if r.SameProcess(target) {
			return true
		}
	}
	return false
}

func interactedAfter(a, b *SessionState) bool {
	if a.LastInteractionTime == nil {
		return false
	}
	if b.LastInteractionTime == nil {
		return true
	}
	return a.LastInteractionTime.After(*b.LastInteractionTime)
}
