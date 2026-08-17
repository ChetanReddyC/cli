package strategy

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/internal/proctree"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// agentAncestryWalkDepth is how far the session-start hook walks its
// ancestry looking for the agent process. The chain above the hook is
// (sh -c wrapper(s)) → agent → user shell; a few levels of slack cover
// agents that interpose more than one wrapper.
const agentAncestryWalkDepth = 6

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

// recordAgentAncestry resolves the ref recorded in SessionState.AgentAncestry:
// the first NON-SHELL ancestor of the session-start hook, and only that one.
// Skipping shells handles agents that spawn hooks via `sh -c`; STOPPING at the
// first non-shell process is what keeps shared user-side infrastructure — the
// terminal shell, tmux, the terminal emulator, an IDE — out of the recorded
// set. Anything above the agent is shared with unrelated processes: a human
// commit typed in the same terminal (or IDE terminal) has the same shell and
// emulator in ITS ancestry, and recording those would falsely identity-match
// it to this session. The residual accepted here: an agent that is itself a
// shell script records the user's shell's parent instead and is documented as
// unsupported for identity matching (no shipped agent is one) — worktree
// matching still covers it.
func recordAgentAncestry() []proctree.ProcessRef {
	for _, ref := range proctree.Ancestors(agentAncestryWalkDepth) {
		if !isShellLikeExe(ref.Exe) {
			return []proctree.ProcessRef{ref}
		}
	}
	return nil
}

// shellLikeExes names executables that host arbitrary child processes rather
// than being one: matching on them proves shared plumbing, not authorship.
// Compared against the basename, lowercased, with a trailing ".exe" trimmed.
var shellLikeExes = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"ksh": true, "csh": true, "tcsh": true, "ash": true, "busybox": true,
	"cmd": true, "powershell": true, "pwsh": true,
	"tmux": true, "screen": true, "login": true,
}

func isShellLikeExe(exe string) bool {
	name := strings.ToLower(filepath.Base(exe))
	name = strings.TrimSuffix(name, ".exe")
	// A login shell's comm is prefixed with "-" (e.g. "-fish").
	name = strings.TrimPrefix(name, "-")
	return shellLikeExes[name]
}
