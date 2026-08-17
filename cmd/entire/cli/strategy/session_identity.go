package strategy

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/internal/proctree"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// agentAncestryWalkDepth is how far the session-initializing hook walks its
// ancestry looking for the agent process. The chain above the hook is
// (sh -c wrapper(s)) → agent → user shell; a few levels of slack cover
// agents that interpose more than one wrapper.
const agentAncestryWalkDepth = 6

// commitAncestryDepth is how far the commit hook walks its own ancestry when
// matching — deeper than the recording walk because agents interpose shells
// and wrappers between themselves and the git subprocess (hook ← git ← tool
// shell(s) ← agent ← ...).
const commitAncestryDepth = 12

// findSessionsForCommitLinking resolves which sessions a commit belongs to:
// the union of the worktree-matched set (a commit captures the worktree's
// content, so every session with pending content here belongs in it —
// concurrent sessions interleave by design) and the identity-matched session
// when the committing process's ancestry names one that worktree matching
// missed. There is no precedence between the two — an identity hit must
// never suppress worktree matches, or concurrent same-worktree sessions
// would drop out of the commit. The identity union is what makes an
// agent-made commit immune to worktree bookkeeping drift: an agent
// committing in a sibling worktree still links to its own session
// (guest-linked), where path matching alone found nothing or declined as
// ambiguous.
//
// This is also the only place the multi-worktree ambiguity decline is
// surfaced to the user: the hint fires only when the FINAL set is empty, so
// a commit rescued by identity matching never sees a false "none was
// linked", and amend/post-rewrite paths (which call findSessionsForWorktree
// directly) stay silent.
func (s *ManualCommitStrategy) findSessionsForCommitLinking(ctx context.Context, worktreePath string) ([]*SessionState, error) {
	allStates, err := s.listAllSessionStates(ctx)
	if err != nil {
		// Identity matching below needs the same listing, so nothing can
		// rescue this; report it to the caller (hooks log and skip).
		return nil, err
	}
	sessions, ambiguous := s.findSessionsForWorktreeFromStates(ctx, allStates, worktreePath)
	if guest := s.findSessionByCommitAncestry(ctx, allStates); guest != nil && !linkingSetContains(sessions, guest.SessionID) {
		sessions = append(sessions, guest)
	}
	if ambiguous && len(sessions) == 0 {
		fmt.Fprintln(stderrWriter,
			"[entire] Agent sessions in several other worktrees could match this commit; none was linked. Run 'entire session adopt' in this worktree to link future commits to your session.")
	}
	return sessions, nil
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
// this commit's ancestry — a nested agent vs. the outer agent that spawned
// it, or an agent vs. the non-shell host recorded for a shell-script agent —
// the one closest to the commit is the author. Ties at the same depth (the
// same live process recorded by several sessions, e.g. a resumed agent) go
// to the most recently interacting session. Imported sessions are historical
// records and adopted-away tombstones belong to another store; neither ever
// matches. Returns nil when nothing in the ancestry is a recorded agent (the
// linking set is then the worktree-matched sessions alone).
func (s *ManualCommitStrategy) findSessionByCommitAncestry(ctx context.Context, states []*SessionState) *SessionState {
	logCtx := logging.WithComponent(ctx, "checkpoint")
	ancestors := proctree.Ancestors(commitAncestryDepth)
	if len(ancestors) == 0 {
		// Systematic on some hosts (restricted /proc, sandbox denials):
		// identity linking never works there and the worktree fallback masks
		// it, so leave a trace for support bundles.
		logging.Debug(logCtx, "commit ancestry unresolvable; identity matching skipped")
		return nil
	}
	if len(states) == 0 {
		return nil
	}

	for _, anc := range ancestors { // nearest-first
		var best *SessionState
		for _, state := range states {
			if state.Kind.IsImported() || state.AdoptedIntoWorktreePath != "" || !refsContain(state.AgentAncestry, anc) {
				continue
			}
			if best == nil || interactedAfter(state, best) {
				best = state
			}
		}
		if best != nil {
			logging.Debug(logCtx, "commit attributed to session by process ancestry",
				slog.String("session_id", best.SessionID),
				slog.Int("ancestor_pid", anc.PID),
				slog.String("ancestor_exe", anc.Exe),
			)
			return best
		}
	}
	return nil
}

// isSessionHomeWorktree reports whether worktreePath — the commit's worktree,
// already resolved by the hook entry point — is the one the session is
// recorded in. Worktree-coupled state (BaseCommit, shadow-branch content and
// deletion) may only be mutated from the session's home worktree; a
// guest-linked commit elsewhere condenses and links without moving it. A
// pure comparison by design: an earlier version re-resolved the worktree
// here and read resolution failure as "home", which would have mutated a
// guest session's state in exactly the way the gate exists to prevent.
func isSessionHomeWorktree(worktreePath string, state *SessionState) bool {
	return worktreePath == "" || state.WorktreePath == "" || state.WorktreePath == worktreePath
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
// the first NON-SHELL ancestor of the session-initializing hook, and only
// that one. Skipping shells handles agents that spawn hooks via `sh -c`;
// STOPPING at the first non-shell process is what keeps shared user-side
// infrastructure — the terminal shell, tmux, the terminal emulator, an IDE —
// out of the recorded set. Anything above the agent is shared with unrelated
// processes: a human commit typed in the same terminal (or IDE terminal) has
// the same shell and emulator in ITS ancestry, and recording those would
// falsely identity-match it to this session. The residual accepted here: an
// agent that is itself a shell script records the user's shell's parent
// instead and is documented as unsupported for identity matching (no shipped
// agent is one) — worktree matching still covers it.
func recordAgentAncestry(ctx context.Context) []proctree.ProcessRef {
	ancestors := proctree.Ancestors(agentAncestryWalkDepth)
	for _, ref := range ancestors {
		if !isShellLikeExe(ref.Exe) {
			return []proctree.ProcessRef{ref}
		}
	}
	// Distinguish the two failure classes — they have different fixes
	// (platform introspection blocked vs. an all-shell chain).
	logCtx := logging.WithComponent(ctx, "checkpoint")
	if len(ancestors) == 0 {
		logging.Debug(logCtx, "agent ancestry not recorded: process ancestry unresolvable; commits will link by worktree only")
	} else {
		logging.Debug(logCtx, "agent ancestry not recorded: all ancestors are shell-like; commits will link by worktree only",
			slog.Int("ancestors_walked", len(ancestors)))
	}
	return nil
}

// refreshAgentAncestry re-records the agent ref when none of the recorded
// refs is an ancestor of the current hook — the session was resumed under a
// new agent process (the old ref is dead and, thanks to the start-time
// guard, can never match again; without a refresh the session would silently
// revert to worktree-only linking forever).
func refreshAgentAncestry(ctx context.Context, state *SessionState) {
	current := proctree.Ancestors(commitAncestryDepth)
	for _, anc := range current {
		if refsContain(state.AgentAncestry, anc) {
			return // recorded agent is still live in our ancestry
		}
	}
	if refs := recordAgentAncestry(ctx); refs != nil {
		state.AgentAncestry = refs
	}
}

// shellLikeExes names executables that host arbitrary child processes rather
// than being one: matching on them proves shared plumbing, not authorship.
// Compared against the basename, lowercased, with a trailing ".exe" trimmed,
// a login-shell "-" prefix stripped, and anything after the first space or
// ':' dropped (a multiplexer may rename itself, e.g. tmux's server reports
// "tmux: server" as its comm).
var shellLikeExes = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true,
	"ksh": true, "csh": true, "tcsh": true, "ash": true, "busybox": true,
	"cmd": true, "powershell": true, "pwsh": true,
	"tmux": true, "screen": true, "login": true,
}

func isShellLikeExe(exe string) bool {
	// An unresolvable name is not evidence of anything — treat it like a
	// shell (skip, keep walking) rather than recording an unidentifiable
	// process as "the agent", which could be the very shell the filter
	// exists to exclude.
	if strings.TrimSpace(exe) == "" {
		return true
	}
	name := strings.ToLower(filepath.Base(exe))
	if i := strings.IndexAny(name, " :"); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSuffix(name, ".exe")
	// A login shell's comm is prefixed with "-" (e.g. "-fish").
	name = strings.TrimPrefix(name, "-")
	if name == "" || name == "." {
		return true
	}
	return shellLikeExes[name]
}
