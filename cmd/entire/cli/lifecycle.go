// lifecycle.go implements the generic lifecycle event dispatcher.
// It routes normalized events from any agent to the appropriate framework actions.
//
// The dispatcher inverts the current flow from "agent handler calls framework functions"
// to "framework dispatcher calls agent methods." Agents are passive data providers;
// the dispatcher handles all orchestration: state transitions, strategy calls,
// file change detection, metadata generation.
package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/codex"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/provenance"
	"github.com/entireio/cli/cmd/entire/cli/review"
	"github.com/entireio/cli/cmd/entire/cli/session"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/entireio/cli/cmd/entire/cli/transcript"
	"github.com/entireio/cli/cmd/entire/cli/validation"
	"github.com/entireio/cli/perf"
)

// eventBypassesAgentOwnershipCheck reports whether an event must run
// regardless of the recorded session-owning agent:
//   - SessionStart fires before SessionState exists; the hint file dedup
//     in handleLifecycleSessionStart already prevents a duplicate banner.
//   - TurnStart needs to reach InitializeSession so transcript-path
//     resolution can repair a wrongly-set AgentType. Skipping here would
//     lock in a bad state.
func eventBypassesAgentOwnershipCheck(t agent.EventType) bool {
	return t == agent.SessionStart || t == agent.TurnStart
}

// DispatchLifecycleEvent routes a normalized lifecycle event to the appropriate handler.
// Returns nil if the event was handled successfully.
func DispatchLifecycleEvent(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	if ag == nil {
		return errors.New("agent cannot be nil")
	}
	if event == nil {
		return errors.New("event cannot be nil")
	}

	// Reject path-unsafe identifiers once, here, before any handler uses them to
	// build filesystem paths. Handlers historically validated individually,
	// which is fragile — handleLifecycleTurnEnd builds .entire/metadata/<id>/
	// via os.MkdirAll + os.WriteFile, and handleLifecycleSubagentEnd builds a
	// subagent transcript path from SubagentID and reads it, without their own
	// checks. Centralizing the guard covers every handler (and any future one)
	// uniformly. Empty IDs pass through: handlers apply their own empty-handling
	// (e.g. TurnEnd falls back to a safe constant; SubagentEnd skips the path).
	if event.SessionID != "" {
		if err := validation.ValidateSessionID(event.SessionID); err != nil {
			return fmt.Errorf("invalid session ID in %s event: %w", event.Type, err)
		}
	}
	if event.ToolUseID != "" {
		if err := validation.ValidateToolUseID(event.ToolUseID); err != nil {
			return fmt.Errorf("invalid tool use ID in %s event: %w", event.Type, err)
		}
	}
	if event.SubagentID != "" {
		if err := validation.ValidateAgentID(event.SubagentID); err != nil {
			return fmt.Errorf("invalid subagent ID in %s event: %w", event.Type, err)
		}
	}

	// Filter forwarded hooks: when Cursor IDE forwards events to both
	// .cursor/hooks.json and .claude/settings.json, only the agent that owns
	// the session should process them — otherwise checkpoints, metadata
	// writes, and step counts double.
	if event.SessionID != "" && !eventBypassesAgentOwnershipCheck(event.Type) {
		if state, _ := strategy.LoadSessionState(ctx, event.SessionID); state != nil && state.AgentType != "" && state.AgentType != ag.Type() { //nolint:errcheck // a load failure means we can't filter; let the event reach its handler, which surfaces its own load error
			logging.Info(logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name()),
				"skipping forwarded hook for non-owning agent",
				slog.String("event", event.Type.String()),
				slog.String("session_id", event.SessionID),
				slog.String("owning_agent", string(state.AgentType)),
				slog.String("firing_agent", string(ag.Type())),
			)
			return nil
		}
	}

	switch event.Type {
	case agent.SessionStart:
		return handleLifecycleSessionStart(ctx, ag, event)
	case agent.TurnStart:
		return handleLifecycleTurnStart(ctx, ag, event)
	case agent.TurnEnd:
		return handleLifecycleTurnEnd(ctx, ag, event)
	case agent.Compaction:
		return handleLifecycleCompaction(ctx, ag, event)
	case agent.SessionEnd:
		return handleLifecycleSessionEnd(ctx, ag, event)
	case agent.SubagentStart:
		return handleLifecycleSubagentStart(ctx, ag, event)
	case agent.SubagentEnd:
		return handleLifecycleSubagentEnd(ctx, ag, event)
	case agent.ModelUpdate:
		return handleLifecycleModelUpdate(ctx, ag, event)
	case agent.ToolUse:
		return handleLifecycleToolUse(ctx, ag, event)
	default:
		return fmt.Errorf("unknown lifecycle event type: %d", event.Type)
	}
}

// handleLifecycleSessionStart handles session start: shows banner, checks concurrent sessions,
// fires state machine transition.
func handleLifecycleSessionStart(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "session-start",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("session_ref", event.SessionRef),
		slog.String("model", event.Model),
	)

	if event.SessionID == "" {
		return fmt.Errorf("no session_id in %s event", event.Type)
	}
	if err := validation.ValidateSessionID(event.SessionID); err != nil {
		return fmt.Errorf("invalid %s event: %w", event.Type, err)
	}

	// Claim the session for this agent. First-writer-wins: subsequent agents
	// firing SessionStart for the same session ID are no-ops. Used by
	// InitializeSession (TurnStart) and the dispatcher skip in
	// DispatchLifecycleEvent for cross-agent disambiguation when Cursor IDE
	// forwards hooks to both .cursor/hooks.json and .claude/settings.json.
	if _, hintErr := strategy.StoreAgentTypeHint(ctx, event.SessionID, ag.Type()); hintErr != nil {
		logging.Warn(logCtx, "failed to store agent hint on session start",
			slog.String("error", hintErr.Error()))
	}

	// Resolve scope before the TurnStart prompt path.
	refreshCtx, refreshCancel := context.WithTimeout(ctx, trailEnablementSessionStartRefreshTimeout)
	if scope, scopeErr := currentTrailEnablementScope(refreshCtx); scopeErr != nil {
		logging.Debug(logCtx, "trails enablement refresh skipped",
			slog.String("error", scopeErr.Error()))
	} else {
		if hintErr := saveTrailEnablementScopeHint(ctx, event.SessionID, scope); hintErr != nil {
			logging.Debug(logCtx, "failed to cache trails scope hint",
				slog.String("error", hintErr.Error()))
		}
		if refreshErr := refreshTrailsEnabledCacheIfStaleForScope(refreshCtx, scope); refreshErr != nil {
			logging.Debug(logCtx, "trails enablement refresh skipped",
				slog.String("error", refreshErr.Error()))
		}
	}
	refreshCancel()

	// Build informational message — warn early if repo has no commits yet,
	// since checkpoints require at least one commit to work.
	message := sessionStartMessage(ag.Name(), false)
	if repo, err := strategy.OpenRepository(ctx); err == nil {
		defer repo.Close()
		if strategy.IsEmptyRepository(repo) {
			message = sessionStartMessage(ag.Name(), true)
		}
	}

	// Check for concurrent sessions and append count if any
	_, countSessionsSpan := perf.Start(ctx, "count_active_sessions")
	strat := GetStrategy(ctx)
	if count, err := strat.CountOtherActiveSessionsWithCheckpoints(ctx, event.SessionID); err == nil && count > 0 {
		if ag.Name() == agent.AgentNameCodex {
			message += fmt.Sprintf(" %d other active conversation(s) in this workspace will also be included. Use 'entire status' for more information.", count)
		} else {
			message += fmt.Sprintf("\n  %d other active conversation(s) in this workspace will also be included.\n  Use 'entire status' for more information.", count)
		}
	}
	countSessionsSpan.End()

	// Codex-only: surface untrusted hooks. Reaching this point means
	// SessionStart is itself trusted, but a newer entire release may have
	// added hooks (e.g. PostToolUse) that the user hasn't approved on
	// this machine. Trust state is keyed by the absolute hooks.json
	// path, so missing entries here flag exactly that case.
	if ag.Name() == agent.AgentNameCodex {
		if root, err := paths.WorktreeRoot(ctx); err == nil {
			if gaps := codex.HookTrustGaps(root); len(gaps) > 0 {
				message += fmt.Sprintf(" %d new hook(s) await approval (%s). Open /hooks to trust them.", len(gaps), strings.Join(gaps, ", "))
			}
		}
	}

	// Output informational message if the agent supports hook responses.
	// Claude Code reads JSON from stdout; agents that don't implement
	// HookResponseWriter silently skip (avoids raw JSON in their terminal).
	//
	// Banner display is gated by ClaimSessionStartBanner — separate from the
	// agent-ownership claim above. If the ownership winner can't write banners
	// (Cursor), we'd suppress the banner entirely on a Cursor+Claude race;
	// the banner marker is only claimed inside this branch so a non-writer
	// winner can't consume the user's only banner.
	_, hookResponseSpan := perf.Start(ctx, "write_hook_response")
	// Apply any agent-supplied ResponseMessage override, then append the
	// agent-help banner pointer so it survives the override — banner-only agents
	// (Factory Droid) have no other in-session channel for it.
	message = finalizeSessionStartBanner(message, event.ResponseMessage, ag.Name())
	if writer, ok := agent.AsHookResponseWriter(ag); ok {
		bannerFirst, bErr := strategy.ClaimSessionStartBanner(ctx, event.SessionID)
		if bErr != nil {
			// Better to duplicate the banner than to suppress the only one.
			logging.Warn(logCtx, "failed to claim session start banner marker",
				slog.String("error", bErr.Error()))
			bannerFirst = true
		}
		if bannerFirst {
			if err := writer.WriteHookResponse(message); err != nil {
				hookResponseSpan.RecordError(err)
				hookResponseSpan.End()
				return fmt.Errorf("failed to write hook response: %w", err)
			}
		}
	}
	hookResponseSpan.End()

	// Store model hint if the agent provided model info on SessionStart
	if event.Model != "" {
		if err := strategy.StoreModelHint(ctx, event.SessionID, event.Model); err != nil {
			logging.Warn(logCtx, "failed to store model hint on session start",
				slog.String("error", err.Error()))
		}
	}

	// Fire EventSessionStart for the current session (if state exists).
	// SessionStart can fire before InitializeSession creates the state file,
	// so ErrStateNotFound is the normal first-session path — only warn on
	// genuinely unexpected errors, matching the rest of this file.
	mutErr := strategy.MutateSessionState(ctx, event.SessionID, func(state *strategy.SessionState) error {
		if state.AdoptedIntoWorktreePath != "" {
			logging.Info(logCtx, "skipping adopted-away source session start",
				slog.String("adopted_into_worktree", state.AdoptedIntoWorktreePath))
			return strategy.ErrMutationSkip
		}
		persistEventMetadataToState(event, state)
		if transErr := strategy.TransitionAndLog(ctx, state, session.EventSessionStart, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
			logging.Warn(logCtx, "session start transition failed",
				slog.String("error", transErr.Error()))
		}
		return nil
	})
	if mutErr != nil && !errors.Is(mutErr, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to update session state on start",
			slog.String("error", mutErr.Error()))
	}

	return nil
}

func sessionStartMessage(agentName types.AgentName, emptyRepo bool) string {
	if agentName == agent.AgentNameCodex {
		if emptyRepo {
			return "Entire CLI found no commits yet — checkpoints will activate after your first commit."
		}
		return "Entire CLI will link this conversation to your next commit."
	}

	if emptyRepo {
		return "\n\nEntire CLI found no commits yet — checkpoints will activate after your first commit."
	}
	return "\n\nEntire CLI will link this conversation to your next commit."
}

// agentHelpBannerSuffix returns the SessionStart banner suffix that points an
// agent at `entire agent-help`. It targets Factory AI Droid, which is banner-only
// — no model-context injection and no agent-help skill file — so the SessionStart
// banner is its sole in-session channel for the pointer. Every other agent gets
// the pointer via context injection (Claude/Codex/Gemini/OpenCode/Pi), a skill
// file (Claude/Codex/Gemini), or the passive `entire status` surface
// (Cursor/Copilot), so this returns "" for them to avoid a duplicate pointer.
func agentHelpBannerSuffix(agentName types.AgentName) string {
	if agentName == agent.AgentNameFactoryAIDroid {
		return fmt.Sprintf("\n  Run `%s` to see entire's commands and flags.", agentHelpCommand)
	}
	return ""
}

// finalizeSessionStartBanner applies an agent-supplied ResponseMessage override
// (if any) and THEN appends the agent-help banner pointer, so the pointer
// survives even when the agent supplies its own banner text. Order matters: a
// ResponseMessage override replaces the assembled message wholesale, so the
// pointer must be appended after it, not before.
func finalizeSessionStartBanner(message, responseMessage string, agentName types.AgentName) string {
	if responseMessage != "" {
		message = responseMessage
	}
	return message + agentHelpBannerSuffix(agentName)
}

// handleLifecycleModelUpdate persists the model name for the current session.
//
// If the session state file already exists (e.g., Gemini's BeforeModel fires
// after TurnStart), the model is written directly to state.ModelName — no hint
// file needed. Otherwise falls back to StoreModelHint for cross-process
// persistence (see its doc comment for the full rationale).
func handleLifecycleModelUpdate(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "model-update",
		slog.String("session_id", event.SessionID),
		slog.String("model", event.Model),
	)

	if event.SessionID == "" || event.Model == "" {
		return nil
	}

	// Prefer writing directly to session state when it exists
	mutErr := strategy.MutateSessionState(ctx, event.SessionID, func(state *strategy.SessionState) error {
		state.ModelName = event.Model
		return nil
	})
	if mutErr == nil {
		return nil
	}
	if !errors.Is(mutErr, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to update session state with model",
			slog.String("error", mutErr.Error()))
		return nil
	}

	// State doesn't exist yet — use hint file (see StoreModelHint doc)
	if err := strategy.StoreModelHint(ctx, event.SessionID, event.Model); err != nil {
		logging.Warn(logCtx, "failed to store model hint",
			slog.String("error", err.Error()))
	}

	return nil
}

// handleLifecycleToolUse merges files reported by a per-tool-use hook into
// the session's FilesTouched. Lightweight by design: no SaveStep, no shadow
// branch commit — just enough so PostCommit's carry-forward decision sees
// an accurate file list mid-turn.
func handleLifecycleToolUse(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())

	if event.SessionID == "" {
		return nil
	}
	if err := validation.ValidateSessionID(event.SessionID); err != nil {
		return fmt.Errorf("invalid %s event: %w", event.Type, err)
	}

	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Outside a repo or repo missing — nothing to track. Don't fail the hook.
		logging.Debug(logCtx, "tool-use: no worktree root, skipping",
			slog.String("session_id", event.SessionID),
			slog.String("error", err.Error()),
		)
		return nil
	}

	modified := normalizeToolUsePaths(event.ModifiedFiles, event.CWD, repoRoot)
	added := normalizeToolUsePaths(event.NewFiles, event.CWD, repoRoot)
	deleted := normalizeToolUsePaths(event.DeletedFiles, event.CWD, repoRoot)

	if len(modified) == 0 && len(added) == 0 && len(deleted) == 0 {
		return nil
	}

	logging.Debug(logCtx, "tool-use: recording files touched",
		slog.String("session_id", event.SessionID),
		slog.Int("modified", len(modified)),
		slog.Int("added", len(added)),
		slog.Int("deleted", len(deleted)),
	)

	if err := strategy.RecordFilesTouched(ctx, event.SessionID, modified, added, deleted); err != nil {
		logging.Warn(logCtx, "tool-use: failed to record files touched",
			slog.String("session_id", event.SessionID),
			slog.String("error", err.Error()),
		)
	}
	return nil
}

// normalizeToolUsePaths converts hook-payload paths to repo-root-relative form.
// Codex apply_patch envelopes carry cwd-relative paths, so we join them against
// eventCWD before FilterAndNormalizePaths rewrites against repoRoot.
func normalizeToolUsePaths(files []string, eventCWD, repoRoot string) []string {
	if len(files) == 0 {
		return nil
	}
	resolved := make([]string, 0, len(files))
	for _, f := range files {
		if f == "" {
			continue
		}
		if filepath.IsAbs(f) || eventCWD == "" {
			resolved = append(resolved, f)
			continue
		}
		resolved = append(resolved, filepath.Join(eventCWD, f))
	}
	return FilterAndNormalizePaths(resolved, repoRoot)
}

// handleLifecycleTurnStart handles turn start: captures pre-prompt state,
// ensures strategy setup, initializes session.
// entireTrailContextInjection is the one-time, model-facing pointer Entire
// injects on the first turn of a session. It points at `entire agent-help` for
// the full flag/subcommand surface — fetched on demand so that surface never goes
// stale here as it grows — and adds only what an agent must know even if it never
// drills in: commits auto-capture checkpoints, and setup/destructive commands
// belong to the user. It also names the auto-detected repo (from the
// already-loaded session scope, no IO) and the standing rule that the agent is
// inside the repo and must never ask the user for the repo name. Kept terse: it
// costs context-window tokens on the first turn of every session.
//
// Deliberately NOT here: per-task command recommendations. An earlier revision
// urged `entire why <file>:<line>` and `entire checkpoint search` "before large
// edits". A census of 963 agent transcripts on a heavy-use machine found zero
// invocations of either against 25 calls to the agent-help pointer above, so the
// recommendation only ever cost tokens. It also mis-framed a
// sometimes-appropriate query as an always-do step. Which commands suit a given
// task is agent-help's job, where it is pulled on demand and grouped by who
// should initiate the command (see agentHelpAudience); this string carries only
// invariants that hold on every turn of every session.
func entireTrailContextInjection(scope trailEnablementScope) string {
	repo := ""
	if scope.Forge != "" && scope.Owner != "" && scope.Repo != "" {
		repo = trailEnablementRepoKey(scope.Forge, scope.Owner, scope.Repo)
	}
	var b strings.Builder
	b.WriteString("Entire is enabled for this repo. Run `entire agent-help` to see what entire does and which subcommand to use, then `entire agent-help <command>` for that command's exact, current flags. ")
	b.WriteString("Commits automatically capture the AI session as a checkpoint, so never create checkpoints by hand — just commit normally. Leave setup and destructive commands (enable, disable, clean, rewind, auth) to the user. ")
	// Mirror agentHelpRepoBlock's defense-in-depth: this string is injected raw
	// into the agent's model context (no escaping), so a repo key carrying control
	// characters (e.g. an <sessionID>.trail-scope.json cache written by a pre-fix
	// binary, or tampered) degrades to the generic message rather than reaching
	// that sink.
	if repo != "" && strings.IndexFunc(repo, unicode.IsControl) < 0 {
		b.WriteString("This repo is auto-detected from the git origin remote as ")
		b.WriteString(repo)
		b.WriteString("; you are already inside it, so never ask the user for the repo name.")
	} else {
		b.WriteString("Entire auto-detects the repo from the git origin remote, so never ask the user for the repo name.")
	}
	return b.String()
}

// emitContextInjection writes ag's native context-injection payload to stdout
// when ag injects at event.Type, trails are enabled for the repo on the API,
// and this session has not been injected yet. Best-effort: an injection failure
// never fails the hook.
func emitContextInjection(ctx context.Context, ag agent.Agent, event *agent.Event) {
	injector, ok := agent.AsContextInjector(ag)
	if !ok || injector.InjectionEvent() != event.Type || event.SessionID == "" {
		return
	}
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())

	// Unknown cache leaves the session retryable.
	scope, scopeOK, scopeErr := loadTrailEnablementScopeHint(ctx, event.SessionID)
	if scopeErr != nil {
		logging.Warn(logCtx, "failed to load trails scope hint",
			slog.String("error", scopeErr.Error()))
		return
	}
	decision := trailEnablementCacheUnknown
	mutated := false
	mutErr := strategy.MutateSessionState(ctx, event.SessionID, func(state *strategy.SessionState) error {
		if state.ContextInjectionDecided {
			return strategy.ErrMutationSkip
		}
		// Review/investigate sessions are task-specific and don't need the branch
		// trail pointer; skip without marking decided so normal sessions keep the
		// usual first-turn behavior.
		if state.Kind != "" {
			return strategy.ErrMutationSkip
		}
		if !scopeOK {
			return strategy.ErrMutationSkip
		}
		decision = cachedTrailsEnablementForScope(ctx, scope, time.Now())
		if decision == trailEnablementCacheUnknown {
			return strategy.ErrMutationSkip
		}
		state.ContextInjectionDecided = true
		mutated = true
		return nil
	})
	if mutErr != nil && !errors.Is(mutErr, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to record context injection decision",
			slog.String("error", mutErr.Error()))
		return
	}
	// Only proceed after the state mutation was persisted. If saving the updated
	// state failed, mutErr was non-nil above and we returned without injecting,
	// leaving a later turn free to retry safely.
	won := mutErr == nil && mutated
	if !won || decision != trailEnablementCacheEnabled {
		return
	}

	payload, err := injector.RenderContextInjection(agent.ContextInjection{Text: entireTrailContextInjection(scope)})
	if err != nil {
		logging.Warn(logCtx, "failed to render context injection",
			slog.String("error", err.Error()))
		return
	}
	if len(payload) == 0 {
		return
	}
	if _, err := os.Stdout.Write(payload); err != nil {
		logging.Warn(logCtx, "failed to write context injection",
			slog.String("error", err.Error()))
	}
}

// turnStartSessionLockWait bounds how long the TurnStart hook waits for the
// per-session state lock. TurnStart fires before the agent runs and must stay
// cheap; its session-state work is best-effort and repaired on the next turn or
// at turn-end. Without a bound, TurnStart blocks on the previous turn's
// still-running checkpoint condensation (which holds the same lock while it
// rewrites the multi-MB transcript), stalling the user's prompt for ~30s. A
// short wait still wins the lock in the common uncontended/brief-contention
// case while degrading gracefully under pathological contention.
const turnStartSessionLockWait = 2 * time.Second

func handleLifecycleTurnStart(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "turn-start",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("session_ref", event.SessionRef),
		slog.String("model", event.Model),
	)

	sessionID := event.SessionID
	if sessionID == "" {
		return fmt.Errorf("no session_id in %s event", event.Type)
	}
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("invalid %s event: %w", event.Type, err)
	}

	// Bound every session-state lock acquisition on the TurnStart path so a
	// background lock holder can't stall the user's prompt (see the const doc).
	ctx = strategy.WithSessionLockWait(ctx, turnStartSessionLockWait)

	// Fill model from hint file if the agent didn't provide it on this hook
	if event.Model == "" {
		if hint := strategy.LoadModelHint(ctx, sessionID); hint != "" {
			event.Model = hint
			logging.Debug(logCtx, "loaded model from hint file",
				slog.String("model", hint))
		}
	}

	// EnsureEntireGitignore can append to the tracked .entire/.gitignore, so run
	// it before CapturePrePromptState: the snapshot should describe the tree the
	// agent starts from, not one setup is about to change.
	_, setupSpan := perf.Start(ctx, "ensure_setup")
	if err := strategy.EnsureSetup(ctx); err != nil {
		logging.Warn(logCtx, "failed to ensure strategy setup",
			slog.String("error", err.Error()))
	}
	setupSpan.End()

	// Capture pre-prompt state (including transcript position via TranscriptAnalyzer)
	_, captureSpan := perf.Start(ctx, "capture_pre_prompt_state")
	if err := CapturePrePromptState(ctx, ag, sessionID, event.SessionRef); err != nil {
		captureSpan.RecordError(err)
		captureSpan.End()
		return err
	}
	captureSpan.End()

	// Append prompt to prompt.txt on filesystem so it's available for
	// mid-turn commits (before SaveStep writes it to the shadow branch).
	// Prompts are separated by "\n\n---\n\n" to support multiple turns.
	if event.Prompt != "" {
		sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
		if sessionDirAbs, absErr := paths.AbsPath(ctx, sessionDir); absErr == nil {
			if mkErr := os.MkdirAll(sessionDirAbs, 0o750); mkErr == nil {
				promptPath := filepath.Join(sessionDirAbs, paths.PromptFileName)
				existing, readErr := os.ReadFile(promptPath) //nolint:gosec // session metadata path
				var content string
				if readErr == nil && len(existing) > 0 {
					content = string(existing) + "\n\n---\n\n" + event.Prompt
				} else {
					content = event.Prompt
				}
				if writeErr := os.WriteFile(promptPath, []byte(content), 0o600); writeErr != nil { //nolint:gosec // path from internal metadata, not user input
					logging.Warn(logCtx, "failed to write prompt.txt",
						slog.String("error", writeErr.Error()))
				}
			}
		}
	}

	// Initialize session (setup already ran above, before the first status read)
	_, initSpan := perf.Start(ctx, "init_session")
	strat := GetStrategy(ctx)
	if err := strat.InitializeSession(ctx, sessionID, ag.Type(), event.SessionRef, event.Prompt, event.Model); err != nil {
		logging.Warn(logCtx, "failed to initialize session state",
			slog.String("error", err.Error()))
	}

	// Best-effort: adopt ENTIRE_REVIEW_* / ENTIRE_INVESTIGATE_* env vars set
	// by `entire review` / `entire investigate` on the spawned agent process.
	// Each agent process has its own env, so there is no file race across
	// worktrees. Errors in load/save must not fail the turn.
	//
	// Review adoption runs first; if both env families are somehow set, review
	// wins. Production strips ENTIRE_REVIEW_* in AppendInvestigateEnv before
	// spawning each per-turn investigate agent process so this conflict cannot
	// happen for fresh investigate spawns. Both functions short-circuit on
	// state.Kind != "" to keep the conflict harmless if it ever arises.
	if mutErr := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
		before := *state
		// Slice fields share their backing array under struct copy. If
		// adoptReviewEnv ever mutates ReviewSkills in place, the diff check
		// below would silently miss it. Clone to keep the comparison honest.
		before.ReviewSkills = slices.Clone(state.ReviewSkills)
		adoptReviewEnv(logCtx, state, string(ag.Name()))
		adoptInvestigateEnv(logCtx, state, string(ag.Name()))

		skillEventSource := *event
		// Record a skill event for a leading "/<command>" in the raw prompt. Only
		// once ownership is known — TurnStart bypasses the owner filter so
		// InitializeSession can repair it — and never overriding native adapter events.
		if state.AgentType == "" || state.AgentType == ag.Type() {
			skillEventSource.SkillEvents = agent.AppendPromptSlashCommandSkillEvent(
				skillEventSource.SkillEvents,
				string(ag.Name()),
				event.Prompt,
				event.Timestamp,
			)
		}
		skillEventsChanged := appendEventSkillEventsToState(&skillEventSource, state)
		if state.Kind == before.Kind &&
			state.ReviewPrompt == before.ReviewPrompt &&
			slices.Equal(state.ReviewSkills, before.ReviewSkills) &&
			state.InvestigateRunID == before.InvestigateRunID &&
			state.InvestigateTopic == before.InvestigateTopic &&
			!skillEventsChanged {
			return strategy.ErrMutationSkip
		}
		return nil
	}); mutErr != nil && !errors.Is(mutErr, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to save session state after review/investigate env adoption",
			slog.String("error", mutErr.Error()))
	}
	initSpan.End()

	// Inject Entire's model-facing context (once per session) for agents whose
	// transport supports it at TurnStart (e.g. Pi). Extension reads stdout.
	emitContextInjection(ctx, ag, event)

	return nil
}

// handleLifecycleTurnEnd handles turn end: validates transcript, extracts metadata,
// detects file changes, saves step + checkpoint, transitions phase.
//
//nolint:maintidx // high complexity due to sequential orchestration of 8 steps (validation, extraction, file detection, filtering, token calc, step save, phase transition, cleanup) - splitting would obscure the flow
func handleLifecycleTurnEnd(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "turn-end",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("session_ref", event.SessionRef),
		slog.String("model", event.Model),
	)

	sessionID := event.SessionID
	if sessionID == "" {
		sessionID = unknownSessionID
	}

	// Fill model from hint file if the agent didn't provide it on this hook
	if event.Model == "" && sessionID != unknownSessionID {
		if hint := strategy.LoadModelHint(ctx, sessionID); hint != "" {
			event.Model = hint
			logging.Debug(logCtx, "loaded model from hint file",
				slog.String("model", hint))
		}
	}

	transcriptRef := event.SessionRef
	if transcriptRef == "" {
		return errors.New("transcript file not specified")
	}

	// If agent implements TranscriptPreparer, materialize the transcript file.
	// This must run BEFORE fileExists: agents like OpenCode lazily fetch transcripts
	// via `opencode export`, so the file doesn't exist until PrepareTranscript creates it.
	// Claude Code's PrepareTranscript just flushes (always succeeds). Agents without
	// TranscriptPreparer (Gemini, Droid) are unaffected.
	_, prepareSpan := perf.Start(ctx, "prepare_and_validate_transcript")
	if preparer, ok := agent.AsTranscriptPreparer(ag); ok {
		if err := preparer.PrepareTranscript(ctx, transcriptRef); err != nil {
			logging.Warn(logCtx, "failed to prepare transcript",
				slog.String("error", err.Error()))
		}
	}

	if !fileExists(transcriptRef) {
		prepareSpan.RecordError(fmt.Errorf("transcript file not found: %s", transcriptRef))
		prepareSpan.End()
		return fmt.Errorf("transcript file not found: %s", transcriptRef)
	}

	// Early check: bail out quickly if the repo has no commits yet.
	// Return nil (not an error) so the hook exits 0 — agents treat non-zero
	// exit codes as hook failures. The user was already warned at session start.
	if repo, err := strategy.OpenRepository(ctx); err == nil {
		defer repo.Close()
		if strategy.IsEmptyRepository(repo) {
			prepareSpan.End()
			logging.Info(logCtx, "skipping checkpoint - will activate after first commit")
			return nil
		}
	}
	prepareSpan.End()

	// Create session metadata directory
	_, copySpan := perf.Start(ctx, "copy_transcript")
	sessionDir := paths.SessionMetadataDirFromSessionID(sessionID)
	sessionDirAbs, err := paths.AbsPath(ctx, sessionDir)
	if err != nil {
		sessionDirAbs = sessionDir
	}
	if err := os.MkdirAll(sessionDirAbs, 0o750); err != nil {
		copySpan.RecordError(err)
		copySpan.End()
		return fmt.Errorf("failed to create session directory: %w", err)
	}

	// Copy transcript to session directory
	transcriptData, err := ag.ReadTranscript(transcriptRef)
	if err != nil {
		copySpan.RecordError(err)
		copySpan.End()
		return fmt.Errorf("failed to read transcript: %w", err)
	}
	// Sanitize before writing: this copy is what the shadow-branch walk blobs and
	// redacts on every Stop. See agent.TranscriptSanitizer for why order matters.
	// The agent's own rollout is untouched.
	storedTranscript := agent.SanitizeTranscriptForStorage(ag, transcriptData)
	logFile := filepath.Join(sessionDirAbs, paths.TranscriptFileName)
	if err := os.WriteFile(logFile, storedTranscript, 0o600); err != nil {
		copySpan.RecordError(err)
		copySpan.End()
		return fmt.Errorf("failed to write transcript: %w", err)
	}
	logging.Debug(logCtx, "copied transcript",
		slog.String("path", sessionDir+"/"+paths.TranscriptFileName),
		slog.Int("raw_bytes", len(transcriptData)),
		slog.Int("stored_bytes", len(storedTranscript)))
	copySpan.End()

	// Load pre-prompt state (captured on TurnStart)
	_, extractSpan := perf.Start(ctx, "extract_metadata")
	preState, err := LoadPrePromptState(ctx, sessionID)
	if err != nil {
		logging.Warn(logCtx, "failed to load pre-prompt state",
			slog.String("error", err.Error()))
	}

	// Determine transcript offset
	transcriptOffset := resolveTranscriptOffset(ctx, preState, sessionID)

	// Backfill prompt.txt from transcript when prompt data is missing.
	// This handles agents whose exec mode doesn't fire UserPromptSubmit (e.g., Factory AI
	// Droid). The transcript is the source of truth — if ExtractPrompts returns nothing,
	// there genuinely were no prompts. We track whether backfill occurred so we can
	// update session state after SaveStep (which may reinitialize state).
	var backfilledPrompt string
	promptPath := filepath.Join(sessionDirAbs, paths.PromptFileName)
	existingPrompt, readPromptErr := os.ReadFile(promptPath) //nolint:gosec // file content is safe session metadata
	if readPromptErr != nil && !os.IsNotExist(readPromptErr) {
		logging.Warn(logCtx, "failed to read prompt.txt, skipping backfill",
			slog.String("error", readPromptErr.Error()))
	} else if len(existingPrompt) == 0 {
		if extractor, ok := agent.AsPromptExtractor(ag); ok {
			prompts, extractErr := extractor.ExtractPrompts(transcriptRef, transcriptOffset)
			if extractErr != nil {
				logging.Warn(logCtx, "failed to extract prompts from transcript",
					slog.String("error", extractErr.Error()))
			} else if len(prompts) > 0 {
				content := strings.Join(prompts, "\n\n---\n\n")
				if writeErr := os.WriteFile(promptPath, []byte(content), 0o600); writeErr != nil {
					logging.Warn(logCtx, "failed to backfill prompt.txt from transcript",
						slog.String("error", writeErr.Error()))
				} else {
					logging.Debug(logCtx, "backfilled prompt.txt from transcript",
						slog.Int("prompt_count", len(prompts)))
					backfilledPrompt = prompts[len(prompts)-1]
				}
			}
		}
	}

	// Compute subagents directory for agents that support subagent extraction.
	subagentsDir := paths.SubagentsDir(filepath.Dir(transcriptRef), event.SessionID)

	// Extract metadata via agent interface (modified files)
	var modifiedFiles []string

	if analyzer, ok := agent.AsTranscriptAnalyzer(ag); ok {
		// Extract modified files - prefer SubagentAwareExtractor if available to include subagent files
		if subagentExtractor, subOk := agent.AsSubagentAwareExtractor(ag); subOk {
			if files, fileErr := subagentExtractor.ExtractAllModifiedFiles(transcriptData, transcriptOffset, subagentsDir); fileErr != nil {
				logging.Warn(logCtx, "failed to extract modified files (with subagents)",
					slog.String("error", fileErr.Error()))
			} else {
				modifiedFiles = files
			}
		} else {
			// Fall back to basic extraction (main transcript only)
			if files, _, fileErr := analyzer.ExtractModifiedFilesFromOffset(transcriptRef, transcriptOffset); fileErr != nil {
				logging.Warn(logCtx, "failed to extract modified files",
					slog.String("error", fileErr.Error()))
			} else {
				modifiedFiles = files
			}
		}
	}
	extractSpan.End()

	// Generate commit message from last prompt (read from session state, set at TurnStart).
	// In exec mode, session state LastPrompt may be empty because UserPromptSubmit never fires.
	// Fall back to backfilledPrompt extracted from the transcript.
	// Single load serves both prompt retrieval and backfill.
	_, commitMsgSpan := perf.Start(ctx, "generate_commit_message")
	lastPrompt := ""
	if sessionState, stateErr := strategy.LoadSessionState(ctx, sessionID); stateErr == nil && sessionState != nil {
		lastPrompt = sessionState.LastPrompt
	}
	// Backfill LastPrompt so `entire status` shows the prompt even when no
	// files were modified (before the early return below).
	if lastPrompt == "" && backfilledPrompt != "" {
		lastPrompt = backfilledPrompt
		mutErr := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
			if state.LastPrompt != "" {
				return strategy.ErrMutationSkip
			}
			state.LastPrompt = backfilledPrompt
			return nil
		})
		if mutErr != nil && !errors.Is(mutErr, strategy.ErrStateNotFound) {
			logging.Warn(logCtx, "failed to backfill LastPrompt in session state",
				slog.String("error", mutErr.Error()))
		}
	}
	commitMessage := generateCommitMessage(lastPrompt, ag.Type())
	logging.Debug(logCtx, "using commit message",
		slog.Int("message_length", len(commitMessage)))
	commitMsgSpan.End()

	// Get worktree root for path normalization
	_, detectSpan := perf.Start(ctx, "detect_file_changes")
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		detectSpan.RecordError(err)
		detectSpan.End()
		return fmt.Errorf("failed to get worktree root: %w", err)
	}

	var preUntrackedFiles []string
	if preState != nil {
		logging.Debug(logCtx, "pre-prompt state",
			slog.Int("pre_existing_untracked_files", len(preState.UntrackedFiles)))
		preUntrackedFiles = preState.PreUntrackedFiles()
	}

	// Detect file changes via git status
	changes, err := DetectFileChanges(ctx, preUntrackedFiles)
	if err != nil {
		logging.Warn(logCtx, "failed to compute file changes",
			slog.String("error", err.Error()))
	}
	detectSpan.End()

	// Filter and normalize all paths
	_, normalizeSpan := perf.Start(ctx, "filter_and_normalize_paths")
	relModifiedFiles := FilterAndNormalizePaths(modifiedFiles, repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)

		// Merge git-status modified files as a fallback for transcript parsing.
		// Transcript parsing is the primary source for modified files, but it can miss
		// files if the agent uses an unrecognized tool or the transcript format changes.
		// Git status catches any tracked file with working-tree changes.
		relModifiedFiles = mergeUnique(relModifiedFiles, FilterAndNormalizePaths(changes.Modified, repoRoot))
	}

	// Filter transcript-extracted files to exclude files already committed to HEAD.
	// When an agent commits files mid-turn, those files are condensed by PostCommit
	// and should not be re-added to FilesTouched by SaveStep. A file is "committed"
	// if it exists in HEAD with the same content as the working tree.
	relModifiedFiles = filterToUncommittedFiles(ctx, relModifiedFiles, repoRoot)
	normalizeSpan.End()

	// Check if there are any changes
	totalChanges := len(relModifiedFiles) + len(relNewFiles) + len(relDeletedFiles)
	if totalChanges == 0 {
		logging.Info(logCtx, "no files modified during session, skipping checkpoint")
		transitionSessionTurnEnd(ctx, sessionID, event)
		if cleanupErr := CleanupPrePromptState(ctx, sessionID); cleanupErr != nil {
			logging.Warn(logCtx, "failed to cleanup pre-prompt state",
				slog.String("error", cleanupErr.Error()))
		}
		// The parent turn itself touched nothing, but a background subagent
		// dispatched earlier may still be running and accumulating changes of
		// its own — the whole point of this backstop. Snapshot it regardless.
		captureInFlightTasks(ctx, ag, sessionID, transcriptRef, false)
		return nil
	}

	// Log file changes
	logFileChanges(ctx, relModifiedFiles, relNewFiles, relDeletedFiles)

	// Get git author
	author, err := GetGitAuthor(ctx)
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Get strategy and agent type
	strat := GetStrategy(ctx)
	agentType := ag.Type()

	// Agents that run subagents as sessions of their own (Factory AI Droid's
	// Workers) reach turn-end here for the subagent's own turn. Attribute that
	// work to the parent task invocation instead of minting a top-level session
	// checkpoint unrelated to the session the user is actually driving.
	if link, isSubagent := resolveSubagentSessionLink(ctx, ag, transcriptRef); isSubagent {
		return saveSubagentSessionTaskStep(ctx, subagentSessionStep{
			link:          link,
			sessionID:     sessionID,
			event:         event,
			transcriptRef: transcriptRef,
			modifiedFiles: relModifiedFiles,
			newFiles:      relNewFiles,
			deletedFiles:  relDeletedFiles,
			author:        author,
			agentType:     agentType,
			strat:         strat,
		})
	}

	// Get transcript position/identifier from pre-prompt state
	var transcriptIdentifierAtStart string
	var transcriptLinesAtStart int
	if preState != nil {
		transcriptIdentifierAtStart = preState.LastTranscriptIdentifier
		transcriptLinesAtStart = preState.TranscriptOffset
	}

	// Resolve token usage. Hook-provided counts (e.g., Cursor's stop hook,
	// which is the only authoritative source for Cursor sessions because the
	// JSONL transcript has no usage fields) take precedence; otherwise fall
	// back to transcript-based computation, preferring SubagentAwareExtractor
	// to include subagent tokens.
	tokenUsage := event.TokenUsage
	if tokenUsage == nil {
		tokenUsage = agent.CalculateTokenUsage(ctx, ag, transcriptData, transcriptLinesAtStart, subagentsDir)
	}

	// Build fully-populated step context and delegate to strategy
	stepCtx := strategy.StepContext{
		SessionID:                sessionID,
		ModifiedFiles:            relModifiedFiles,
		NewFiles:                 relNewFiles,
		DeletedFiles:             relDeletedFiles,
		MetadataDir:              sessionDir,
		MetadataDirAbs:           sessionDirAbs,
		CommitMessage:            commitMessage,
		TranscriptPath:           transcriptRef,
		AuthorName:               author.Name,
		AuthorEmail:              author.Email,
		AgentType:                agentType,
		StepTranscriptIdentifier: transcriptIdentifierAtStart,
		StepTranscriptStart:      transcriptLinesAtStart,
		TokenUsage:               tokenUsage,
	}

	if err := strat.SaveStep(ctx, stepCtx); err != nil {
		return fmt.Errorf("failed to save step: %w", err)
	}

	// Update session state with backfilled prompt after SaveStep.
	// Done after SaveStep because SaveStep may reinitialize session state,
	// which would overwrite an earlier LastPrompt update.
	if backfilledPrompt != "" {
		mutErr := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
			if state.LastPrompt != "" {
				return strategy.ErrMutationSkip
			}
			state.LastPrompt = backfilledPrompt
			return nil
		})
		if mutErr != nil && !errors.Is(mutErr, strategy.ErrStateNotFound) {
			logging.Warn(logCtx, "failed to backfill LastPrompt in session state",
				slog.String("error", mutErr.Error()))
		}
	}

	// Transition session phase and cleanup
	transitionSessionTurnEnd(ctx, sessionID, event)
	if cleanupErr := CleanupPrePromptState(ctx, sessionID); cleanupErr != nil {
		logging.Warn(logCtx, "failed to cleanup pre-prompt state",
			slog.String("error", cleanupErr.Error()))
	}

	// Backstop: snapshot any background subagent still in flight. See
	// captureInFlightTasks.
	captureInFlightTasks(ctx, ag, sessionID, transcriptRef, false)

	return nil
}

// handleLifecycleCompaction handles context compaction: saves current progress
// but stays in ACTIVE phase (unlike TurnEnd which transitions to IDLE).
// Also resets the transcript offset since the transcript may be truncated.
func handleLifecycleCompaction(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "compaction",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
	)

	// Fire EventCompaction to trigger ActionCondenseIfFilesTouched (stays in ACTIVE)
	mutErr := strategy.MutateSessionState(ctx, event.SessionID, func(state *strategy.SessionState) error {
		persistEventMetadataToState(event, state)
		if transErr := strategy.TransitionAndLog(ctx, state, session.EventCompaction, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
			logging.Warn(logCtx, "compaction transition failed",
				slog.String("error", transErr.Error()))
		}
		return nil
	})
	if mutErr != nil && !errors.Is(mutErr, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to save session state after compaction",
			slog.String("error", mutErr.Error()))
	}

	logging.Info(logCtx, "context compaction detected")
	return nil
}

// handleLifecycleSessionEnd handles session end: marks the session as ended.
func handleLifecycleSessionEnd(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "session-end",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
	)

	if event.SessionID == "" {
		return nil // No session to update
	}

	// Note: We intentionally don't clean up cached transcripts here.
	// Post-session commits (carry-forward in ENDED phase) may still need
	// the transcript to extract file changes. Cleanup is handled by
	// `entire clean` or when the session state is fully removed.

	// Finalize any background subagent still in flight BEFORE endSessionNow:
	// endSessionNow marks the session ended and immediately runs the eager
	// condense, which sweeps whatever task steps already exist on the shadow
	// branch. Capturing after it would mint post-condensation shadow data
	// nothing then condenses — the zombie class handleSubagentStopFinal's
	// late-arrival guard exists to prevent. See captureInFlightTasks.
	// NOTE: this runs ahead of the session-end condense deadline
	// (sessionEndCondenseDeadline) that budget-capped agents get; Claude Code
	// sets no budget, and for agents that do, bounding the final captures
	// against the same deadline is a known follow-up.
	captureInFlightTasks(ctx, ag, event.SessionID, event.SessionRef, true)

	if _, err := endSessionNow(ctx, event, event.SessionID, nil, sessionEndCondenseDeadline(ag), endedNow); err != nil {
		logging.Warn(logCtx, "failed to mark session ended",
			slog.String("error", err.Error()))
	}

	return nil
}

// processStart approximates when this hook process began. Package
// initialization runs before main, so it is within milliseconds of exec —
// precise enough to bound work against a deadline the agent measures from the
// moment it spawned us.
var processStart = time.Now()

// sessionEndCondenseDeadline returns the wall-clock instant by which the eager
// condense must be done, for agents that run session-end inside their own
// shutdown under a hard cap (see agent.SessionEndBudgeter). The zero time means
// no deadline.
func sessionEndCondenseDeadline(ag agent.Agent) time.Time {
	budgeter, ok := agent.AsSessionEndBudgeter(ag)
	if !ok {
		return time.Time{}
	}
	budget := budgeter.SessionEndBudget()
	if budget <= 0 {
		return time.Time{}
	}
	return processStart.Add(budget)
}

// endSessionNow runs the canonical "this session is over" sequence: it marks the
// session ended (firing the SessionStop transition → PhaseEnded + EndedAt) and
// eagerly condenses its pending work so PostCommit need not. This prevents
// zombie ENDED sessions from accumulating and causing O(N) overhead on every
// future commit (GitHub issue #591). It is shared by the SessionStop hook
// (handleLifecycleSessionEnd) and the exited-session sweep
// (finalizeExitedSessions), so the two stay in lockstep.
//
// The condense is fail-open (PostCommit retries on the next commit); an error
// marking the session ended is returned so callers can react, and skips the
// condense since the state may be inconsistent. event may be nil when no hook
// event drives the end (the sweep), which skips event-metadata persistence.
// guard is forwarded to markSessionEnded (see there); when it skips the end,
// the condense is skipped too and ended is false.
//
// condenseDeadline, when non-zero, bounds only the condense — never the
// mark-ended write, so the cheap step that un-sticks the session from `entire
// status` is never the one given up on. The bound is best-effort: it cancels git
// subprocesses and any context-aware step, but condensation does not poll ctx
// between stages, so it curtails rather than guarantees. Its purpose is to stop
// short of a host that kills the hook's whole process tree (Codex) rather than
// to make condensation interruptible.
//
// Leaving mark-ended unbounded is not the same as guaranteeing it. It runs under
// MutateSessionState, whose flock acquire blocks (WithSessionLockWait is opt-in,
// and only TurnStart opts in), so a concurrent turn-end condense holding the
// same per-session lock can push it past the host's cap and get the whole tree
// killed. The exited-owner sweep is the backstop for that: the session is
// reclaimed on the next `entire status` / `entire doctor`.
//
// Losing the race costs duplication, not data. One window is worth knowing:
// CondenseSession commits the checkpoint to entire/checkpoints/v1 inside the
// MutateSessionState callback, and the state is saved only after that callback
// returns. A kill in between leaves the checkpoint committed with
// CheckpointTranscriptStart / LastCheckpointID / StepCount / FullyCondensed
// un-advanced, so PostCommit mints a fresh checkpoint ID over the same
// transcript range. Everywhere else, an incomplete condense simply leaves
// FullyCondensed false and PostCommit retries.
func endSessionNow(ctx context.Context, event *agent.Event, sessionID string, guard func(*strategy.SessionState) bool, condenseDeadline time.Time, when endedAtPolicy) (ended bool, err error) {
	ended, err = markSessionEnded(ctx, event, sessionID, guard, when)
	if err != nil || !ended {
		return ended, err
	}
	logCtx := logging.WithComponent(ctx, "lifecycle")
	if !condenseDeadline.IsZero() {
		if remaining := time.Until(condenseDeadline); remaining <= 0 {
			logging.Info(logCtx, "skipping eager condense: session-end budget already spent",
				slog.String("session_id", sessionID))
			return true, nil
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, condenseDeadline)
		defer cancel()
	}
	if condErr := GetStrategy(ctx).CondenseAndMarkFullyCondensed(ctx, sessionID); condErr != nil {
		logging.Warn(logCtx, "eager condense on session end failed",
			slog.String("session_id", sessionID),
			slog.String("error", condErr.Error()))
	}
	return true, nil
}

// handleLifecycleSubagentStart handles subagent start: captures pre-task state.
func handleLifecycleSubagentStart(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	logging.Info(logCtx, "subagent started",
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("tool_use_id", event.ToolUseID),
		slog.String("transcript", event.SessionRef),
	)

	// Capture pre-task state
	if err := CapturePreTaskState(ctx, event.ToolUseID); err != nil {
		return fmt.Errorf("failed to capture pre-task state: %w", err)
	}

	return nil
}

// handleLifecycleSubagentEnd handles subagent completion. It dispatches on
// event.Final — never on any payload sentinel — to tell a real completion
// signal (SubagentStop) from the launch-time PostToolUse (post-task) stub:
//
//   - event.Final == true (SubagentStop): the authoritative final capture.
//     See handleSubagentStopFinal.
//   - event.Final == false, background launch (run_in_background: true in
//     ToolInput): post-task fires seconds after launch, before any real work
//     happens. Records an in-flight marker and defers the real capture to
//     SubagentStop instead of saving a stub task step.
//   - event.Final == false, foreground: unchanged legacy behavior — captures
//     immediately via captureSubagentTaskStep.
func handleLifecycleSubagentEnd(ctx context.Context, ag agent.Agent, event *agent.Event) error {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())
	if event.SubagentType == "" && event.TaskDescription == "" {
		// Extract subagent type and description from tool input
		event.SubagentType, event.TaskDescription = ParseSubagentTypeAndDescription(event.ToolInput)
	}

	if event.Final {
		return handleSubagentStopFinal(logCtx, ag, event)
	}

	if isBackgroundLaunch(logCtx, event.ToolInput) {
		return recordInFlightTaskLaunch(logCtx, event)
	}

	return captureSubagentTaskStep(logCtx, ag, event, subagentCaptureOptions{})
}

// recordInFlightTaskLaunch handles a background Task launch. It records an
// in-flight marker on session state and returns without saving a task step;
// the real capture happens at SubagentStop (handleSubagentStopFinal), which
// is the first point that sees the subagent's actual work. Tolerates
// strategy.ErrStateNotFound the way SaveTaskStep itself tolerates a launch
// event arriving before session state exists.
func recordInFlightTaskLaunch(logCtx context.Context, event *agent.Event) error {
	logging.Debug(logCtx, "background subagent launch detected; deferring capture to subagent-stop",
		slog.String("session_id", event.SessionID),
		slog.String("tool_use_id", event.ToolUseID),
		slog.String("agent_id", event.SubagentID),
	)

	mutErr := strategy.MutateSessionState(logCtx, event.SessionID, func(state *strategy.SessionState) error {
		state.AddTaskRecord(session.TaskRecord{
			ToolUseID:       event.ToolUseID,
			AgentID:         event.SubagentID,
			StartedAt:       time.Now(),
			SubagentType:    event.SubagentType,
			TaskDescription: event.TaskDescription,
		})
		return nil
	})
	switch {
	case errors.Is(mutErr, strategy.ErrStateNotFound):
		logging.Info(logCtx, "no session state to record in-flight marker on; background task will not be captured",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID))
	case mutErr != nil:
		logging.Warn(logCtx, "failed to record in-flight task marker",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID),
			slog.String("error", mutErr.Error()))
	}
	return nil
}

// claimTaskRecord atomically completes the record for toolUseID, reporting
// whether it was live (present and not yet completed). This is the single
// choke point every Final-path capture (SubagentStop's handleSubagentStopFinal
// and the SessionEnd final capture, captureInFlightTaskFinal) goes through, so
// that two Final events racing for the same ToolUseID — a late SubagentStop
// arriving just as SessionEnd sweeps in-flight tasks, or a duplicate
// SubagentStop delivery — capture exactly once: whichever call observes
// claimed==true proceeds with the save, the other sees claimed==false and
// takes the same skip path as the pre-existing foreground/duplicate dedup.
//
// Unlike the prior claim-and-remove model, completing the record here does
// NOT delete it: the record must persist for the future condensation
// materializer (see the durable-records plan). CompleteTaskRecord's
// already-completed check is what gives this the same exactly-once guarantee
// the old remove-based claim provided.
//
// Loss semantics: a capture that fails after a successful claim leaves the
// record completed with no additional fields populated — accepted; a later
// completion attempt will see it already completed and skip.
//
// The returned record is the PRE-completion snapshot (its CompletedAt is
// still zero) — read before CompleteTaskRecord runs, so callers can label
// the capture (SubagentType/TaskDescription/AgentID) from what the record
// carried at launch time.
//
// Tolerates strategy.ErrStateNotFound (the session state may already be gone,
// e.g. an ended/swept session) the same way SaveTaskStep tolerates a missing
// state.
func claimTaskRecord(logCtx context.Context, sessionID, toolUseID string) (session.TaskRecord, bool) {
	var claimed session.TaskRecord
	found := false
	mutErr := strategy.MutateSessionState(logCtx, sessionID, func(state *strategy.SessionState) error {
		if marker := state.FindTaskRecord(toolUseID); marker != nil {
			claimed = *marker
		}
		found = state.CompleteTaskRecord(toolUseID, time.Now())
		return nil
	})
	if mutErr != nil && !errors.Is(mutErr, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to claim task record",
			slog.String("session_id", sessionID),
			slog.String("tool_use_id", toolUseID),
			slog.String("error", mutErr.Error()))
	}
	return claimed, found
}

// handleSubagentStopFinal is the authoritative final capture, run for
// SubagentStop (event.Final == true) and, via captureInFlightTaskFinal, for
// SessionEnd's sweep of any task still in flight when the session closes. It
// guards against a late SubagentStop resurrecting an ended/swept session —
// SaveTaskStep -> ensureSessionInitialized -> initializeSession would
// otherwise re-create session state unconditionally and mint a shadow branch
// nothing condenses, the exact zombie class the session sweep exists to
// prevent — then captures (bypassing the no-changes skip gate: a read-only
// subagent still produced a transcript worth a task step) and completes the
// record via claimTaskRecord, which also closes the race between two Final
// events for the same ToolUseID (see its doc comment).
//
// The state loaded at the top of the function is only good for the zombie
// guard above (state missing entirely) and for logging: it predates
// claimTaskRecord and the capture that follows, and a racing SessionEnd can
// land during that window (both serialize on the per-session gate, so the
// interleaving reduces to ordering). The eager-condense decision near the end
// of the function therefore reloads session state and decides on that fresh
// phase, never on this initial snapshot.
func handleSubagentStopFinal(logCtx context.Context, ag agent.Agent, event *agent.Event) error {
	state, err := strategy.LoadSessionState(logCtx, event.SessionID)
	if err != nil {
		// A real load failure (corrupt/unreadable state file) — distinct from
		// the state simply not existing, for which LoadSessionState returns
		// (nil, nil) and the branch below logs truthfully.
		logging.Warn(logCtx, "skipping subagent-stop capture: failed to load session state",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID),
			slog.String("error", err.Error()))
		return nil
	}
	if state == nil {
		// Session state missing entirely (ended and swept, or never existed).
		// The subagent transcript remains on disk in the agent's own directory;
		// there is nothing here to attach it to, and re-creating session state
		// for a late event would resurrect a zombie session. Nothing to claim
		// either: claimTaskRecord would just no-op against the same missing
		// state.
		logging.Info(logCtx, "skipping subagent-stop capture: session state not found",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID))
		return nil
	}

	marker, claimed := claimTaskRecord(logCtx, event.SessionID, event.ToolUseID)
	if !claimed {
		// No marker claimed: this ToolUseID was already captured at
		// launch-time post-task (foreground task), or another Final event for
		// the same ToolUseID (a duplicate SubagentStop, or a race against the
		// SessionEnd final capture) already claimed and captured it. Skip.
		// An event carrying a SubagentID or a subagent transcript path is the
		// louder variant: those fields mean a real subagent completed, so an
		// unclaimed skip is either the expected foreground dedup, a duplicate
		// event, or a misintegrated agent that sets Final without ever
		// emitting the launch-time marker — worth surfacing over Debug.
		if event.SubagentID != "" || event.SubagentTranscript != "" {
			logging.Warn(logCtx, "no in-flight marker for completed subagent — foreground dedup, a duplicate event, or a misintegrated agent setting Final without launch markers",
				slog.String("session_id", event.SessionID),
				slog.String("tool_use_id", event.ToolUseID),
				slog.String("agent_id", event.SubagentID))
			return nil
		}
		logging.Debug(logCtx, "no in-flight marker claimed for subagent-stop; skipping duplicate/foreground/racing capture",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID))
		return nil
	}

	// SubagentStop payloads carry no tool_input, so event.SubagentType/
	// TaskDescription/SubagentID are typically empty at this point; the marker
	// recorded all three at launch time. Each field falls back independently —
	// not as an all-or-nothing pair — so a marker that only captured one of
	// them (e.g. a legacy marker written before a field existed) still
	// contributes what it has instead of being discarded wholesale.
	if marker.SubagentType != "" {
		event.SubagentType = marker.SubagentType
	}
	if marker.TaskDescription != "" {
		event.TaskDescription = marker.TaskDescription
	}
	if event.SubagentID == "" && marker.AgentID != "" {
		event.SubagentID = marker.AgentID
	}

	// The record is already completed at this point regardless of what
	// captureSubagentTaskStep does below — a capture failure after a
	// successful claim leaves the record completed with no additional fields
	// populated (accepted; see claimTaskRecord).
	//
	// analyzerFilesOnly: true because reaching this point means a marker WAS
	// claimed above — every Final capture that runs through this function is a
	// background task (foreground tasks capture immediately at launch and are
	// never marked in-flight), so the worktree-wide DetectFileChanges scan
	// would risk sweeping in the parent's or another agent's later edits. See
	// subagentCaptureOptions.analyzerFilesOnly.
	captureErr := captureSubagentTaskStep(logCtx, ag, event, subagentCaptureOptions{bypassNoChangesSkip: true, analyzerFilesOnly: true})
	if captureErr != nil {
		return captureErr
	}

	// The eager-condense decision below must use the FRESH phase, not the
	// snapshot loaded at the top of this function. That snapshot is only
	// valid for the zombie guard (missing state) and logging above — the
	// capture we just did (claimTaskRecord + captureSubagentTaskStep) is
	// exactly the window where a racing SessionEnd can flip the session to
	// PhaseEnded (both serialize on the per-session gate, so this reduces to
	// ordering). Deciding on the stale pre-capture phase would miss that
	// transition and skip the eager condense, leaving the task step we just
	// wrote as post-condensation zombie shadow data.
	freshPhase := state.Phase
	if freshState, reloadErr := strategy.LoadSessionState(logCtx, event.SessionID); reloadErr != nil {
		// A read hiccup here must not silently skip the condense decision: if
		// the stale snapshot already said ended, fall back to it rather than
		// treating the reload failure as "not ended".
		logging.Warn(logCtx, "failed to reload session state after subagent-stop capture; falling back to pre-capture phase for condense decision",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID),
			slog.String("error", reloadErr.Error()))
	} else if freshState != nil {
		freshPhase = freshState.Phase
	}
	// freshState == nil (state swept between capture and reload) falls back
	// to the pre-capture phase for the same fail-conservative reason.

	if freshPhase == session.PhaseEnded {
		// The session ended before or during this SubagentStop's capture.
		// Trigger the same eager condense SessionEnd uses so the
		// newly-captured task step doesn't linger as post-condensation
		// zombie shadow data.
		if condErr := GetStrategy(logCtx).CondenseAndMarkFullyCondensed(logCtx, event.SessionID); condErr != nil {
			logging.Warn(logCtx, "eager condense after late subagent-stop capture failed",
				slog.String("session_id", event.SessionID),
				slog.String("error", condErr.Error()))
		}
	}

	return nil
}

// subagentCaptureOptions controls captureSubagentTaskStep's behavior across
// its two callers (launch-time foreground capture vs. SubagentStop final
// capture).
type subagentCaptureOptions struct {
	// bypassNoChangesSkip, when true, saves a task step even when no file
	// changes were detected. Only set for Final (SubagentStop) captures: a
	// read-only background subagent (e.g. a reviewer) still produced a
	// transcript worth a task step, and this is the only chance to capture it.
	bypassNoChangesSkip bool

	// analyzerFilesOnly, when true, skips the whole-worktree
	// LoadPreTaskState/DetectFileChanges merge in captureSubagentTaskStep and
	// captures only event.ModifiedFiles plus the transcript-analyzer-extracted
	// files. Set ONLY for background Final (SubagentStop) captures — see the
	// comment on that skip in captureSubagentTaskStep for the attribution
	// rationale. Never set for the foreground launch-time path, which keeps
	// its original (correct, worktree-scan-based) behavior unchanged.
	analyzerFilesOnly bool
}

// captureSubagentTaskStep detects changes and saves a task checkpoint for a
// completed subagent invocation.
func captureSubagentTaskStep(logCtx context.Context, ag agent.Agent, event *agent.Event, opts subagentCaptureOptions) error {
	// Determine subagent transcript path (empty when the agent stores none).
	// event.SubagentTranscript is authoritative when the hook payload supplies
	// it directly (e.g. Claude Code's SubagentStop carries agent_transcript_path);
	// ResolveAgentTranscriptPath is the fallback for launch-time PostToolUse
	// events, which don't carry it.
	subagentTranscriptPath := event.SubagentTranscript
	if subagentTranscriptPath != "" {
		if _, statErr := os.Stat(subagentTranscriptPath); statErr != nil {
			// Still used as-is below (downstream reads fail soft), but a
			// payload-supplied path that doesn't exist means the agent handed
			// us something wrong — surface it rather than silently storing a
			// transcript-less step.
			logging.Warn(logCtx, "payload-supplied subagent transcript path does not exist",
				slog.String("session_id", event.SessionID),
				slog.String("tool_use_id", event.ToolUseID),
				slog.String("subagent_transcript", subagentTranscriptPath),
				slog.String("error", statErr.Error()))
		}
	} else {
		subagentTranscriptPath = ResolveAgentTranscriptPath(filepath.Dir(event.SessionRef), event.SessionID, event.SubagentID)
	}

	// Log context
	subagentEndAttrs := []any{
		slog.String("event", event.Type.String()),
		slog.String("session_id", event.SessionID),
		slog.String("tool_use_id", event.ToolUseID),
	}
	if event.SubagentID != "" {
		subagentEndAttrs = append(subagentEndAttrs, slog.String("agent_id", event.SubagentID))
	}
	if subagentTranscriptPath != "" {
		subagentEndAttrs = append(subagentEndAttrs, slog.String("subagent_transcript", subagentTranscriptPath))
	}
	logging.Info(logCtx, "subagent completed", subagentEndAttrs...)

	// Extract modified files from hook payload and/or subagent transcript
	var modifiedFiles []string
	modifiedFiles = append(modifiedFiles, event.ModifiedFiles...)
	switch analyzer, ok := agent.AsTranscriptAnalyzer(ag); {
	case !ok:
		// No analyzer: modifiedFiles stays event.ModifiedFiles only.
	case opts.analyzerFilesOnly && subagentTranscriptPath == "":
		// Background Final capture with no resolvable subagent transcript:
		// falling back to scanning event.SessionRef (the PARENT transcript)
		// from offset 0 would attribute the whole session's file activity to
		// this one background task. Skip the analyzer scan entirely — the
		// capture proceeds with only event.ModifiedFiles (typically empty,
		// yielding a transcript-less, file-less task step). The foreground
		// path (analyzerFilesOnly unset) keeps the parent-scan fallback: there
		// the worktree-diff merge below dominates the file lists anyway.
		logging.Warn(logCtx, "subagent transcript unresolvable; final capture proceeding without file attribution",
			slog.String("session_id", event.SessionID),
			slog.String("tool_use_id", event.ToolUseID),
			slog.String("agent_id", event.SubagentID))
	default:
		transcriptToScan := event.SessionRef
		if subagentTranscriptPath != "" {
			transcriptToScan = subagentTranscriptPath
		}
		files, _, fileErr := analyzer.ExtractModifiedFilesFromOffset(transcriptToScan, 0)
		switch {
		case fileErr != nil && opts.analyzerFilesOnly:
			// The analyzer scan is this capture's ONLY file source (no
			// worktree-diff backup in analyzer-only mode), so a transient
			// read error here must fail the capture rather than save a
			// clean-looking zero-file checkpoint that permanently misstates
			// the task as read-only. The record was already claimed (completed)
			// by the caller, so this task's capture is lost — the accepted
			// claim-loss semantics documented at the claim site
			// (claimTaskRecord); the SessionEnd sweep does not retry it.
			logging.Warn(logCtx, "failed to extract modified files from subagent; aborting final capture",
				slog.String("session_id", event.SessionID),
				slog.String("tool_use_id", event.ToolUseID),
				slog.String("error", fileErr.Error()))
			return fmt.Errorf("extract modified files from subagent transcript: %w", fileErr)
		case fileErr != nil:
			// Foreground path: the worktree-diff merge below is the backup
			// file source, so the capture proceeds without the analyzer.
			logging.Warn(logCtx, "failed to extract modified files from subagent",
				slog.String("error", fileErr.Error()))
		default:
			modifiedFiles = mergeUnique(modifiedFiles, files)
		}
	}

	// Load pre-task state and detect file changes.
	// If no pre-task state exists (agent doesn't support pre-task hook), fall back
	// to the session's pre-prompt state. Without either, DetectFileChanges receives
	// nil and treats ALL untracked files as new — which would create spurious task
	// checkpoints for pre-existing untracked files (e.g., .github/hooks/entire.json).
	//
	// Skipped entirely when opts.analyzerFilesOnly is set. DetectFileChanges is a
	// git-status scan of the ENTIRE worktree against the launch-time pre-task
	// baseline. For a foreground capture that's correct: the parent is blocked on
	// the subagent, so the worktree delta since launch really is the subagent's.
	// For a background Final (SubagentStop/SessionEnd) capture, launch and
	// completion can be minutes to hours apart, so the same scan sweeps in
	// whatever the parent — or any other concurrent agent — changed in the
	// meantime, misattributing it to this task's checkpoint. Falling back to only
	// event.ModifiedFiles plus the analyzer-extracted files can under-capture a
	// subagent's shell side-effect files that its transcript never names, and
	// deletions by the subagent are also uncapturable in analyzer-only mode
	// (only the worktree scan detects them), but those are the lesser
	// failures: over-capture steals attribution from someone else's work,
	// which is worse and harder to notice. This mirrors the turn-end
	// incremental path (captureInFlightTaskIncremental), which is already
	// analyzer-only for the same reason.
	var changes *FileChanges
	if !opts.analyzerFilesOnly {
		preState, preErr := LoadPreTaskState(logCtx, event.ToolUseID)
		if preErr != nil {
			logging.Warn(logCtx, "failed to load pre-task state",
				slog.String("error", preErr.Error()))
		}
		var preUntrackedFiles []string
		if preState != nil {
			preUntrackedFiles = preState.PreUntrackedFiles()
		}
		var changesErr error
		changes, changesErr = DetectFileChanges(logCtx, preUntrackedFiles)
		if changesErr != nil {
			logging.Warn(logCtx, "failed to compute file changes",
				slog.String("error", changesErr.Error()))
		}
	}

	// Get worktree root and normalize paths
	repoRoot, err := paths.WorktreeRoot(logCtx)
	if err != nil {
		return fmt.Errorf("failed to get worktree root: %w", err)
	}

	// The transcript records what the subagent wrote at some point in its run, not
	// what is still uncommitted. When the subagent committed its own work mid-turn
	// (the scenario TestSingleSessionSubagentCommitInTurn covers), that commit has
	// already condensed the session and deleted the shadow branch, so there is
	// nothing left to snapshot. Keeping those paths defeats the "no changes, skip"
	// gate below and mints a *new* shadow branch after condensation — which nothing
	// then condenses away, because turn-end skips when no files changed, so it
	// outlives the session.
	//
	// filterToUncommittedFiles is the same guard the turn-end path already applies
	// for this exact reason; it fails open, so a git error keeps the list as-is
	// rather than silently dropping a real checkpoint.
	relModifiedFiles := filterToUncommittedFiles(logCtx, FilterAndNormalizePaths(modifiedFiles, repoRoot), repoRoot)
	var relNewFiles, relDeletedFiles []string
	if changes != nil {
		// changes come from git status, so they are uncommitted by construction.
		relNewFiles = FilterAndNormalizePaths(changes.New, repoRoot)
		relDeletedFiles = FilterAndNormalizePaths(changes.Deleted, repoRoot)
		relModifiedFiles = mergeUnique(relModifiedFiles, FilterAndNormalizePaths(changes.Modified, repoRoot))
	}

	// If no changes, skip — unless this is a Final (SubagentStop) capture: a
	// read-only background subagent (e.g. a reviewer) still produced a
	// transcript worth a task step, and SubagentStop is the only chance to
	// capture it (bypassNoChangesSkip is never set for the foreground
	// launch-time path, which keeps its original skip-on-no-changes behavior).
	if len(relModifiedFiles) == 0 && len(relNewFiles) == 0 && len(relDeletedFiles) == 0 {
		if !opts.bypassNoChangesSkip {
			logging.Info(logCtx, "no file changes detected, skipping task checkpoint")
			_ = CleanupPreTaskState(logCtx, event.ToolUseID) //nolint:errcheck // best-effort cleanup
			return nil
		}
		logging.Info(logCtx, "no file changes detected but capturing anyway (final subagent-stop capture)")
	}

	// Find checkpoint UUID from main transcript (best-effort)
	var checkpointUUID string
	// Use the existing CLI-level checkpoint UUID finder
	mainLines, _ := parseTranscriptForCheckpointUUID(event.SessionRef) //nolint:errcheck // best-effort
	if mainLines != nil {
		checkpointUUID, _ = FindCheckpointUUID(mainLines, event.ToolUseID)
	}

	// Get git author
	author, err := GetGitAuthor(logCtx)
	if err != nil {
		return fmt.Errorf("failed to get git author: %w", err)
	}

	// Build task checkpoint context
	strat := GetStrategy(logCtx)
	agentType := ag.Type()

	taskStepCtx := strategy.TaskStepContext{
		SessionID:              event.SessionID,
		ToolUseID:              event.ToolUseID,
		AgentID:                event.SubagentID,
		ModifiedFiles:          relModifiedFiles,
		NewFiles:               relNewFiles,
		DeletedFiles:           relDeletedFiles,
		TranscriptPath:         event.SessionRef,
		SubagentTranscriptPath: subagentTranscriptPath,
		CheckpointUUID:         checkpointUUID,
		AuthorName:             author.Name,
		AuthorEmail:            author.Email,
		SubagentType:           event.SubagentType,
		TaskDescription:        event.TaskDescription,
		AgentType:              agentType,
	}

	if err := strat.SaveTaskStep(logCtx, taskStepCtx); err != nil {
		return fmt.Errorf("failed to save task step: %w", err)
	}

	_ = CleanupPreTaskState(logCtx, event.ToolUseID) //nolint:errcheck // best-effort cleanup
	return nil
}

// maxInFlightTasksPerCapture bounds how many in-flight background tasks a
// single turn-end captureInFlightTasks invocation processes, selected by
// selectInFlightTasksForSnapshot (least-recently-attempted first, so the
// selection rotates turn-end to turn-end rather than always picking the same
// oldest-launched markers). Applies ONLY to the incremental (final == false)
// path.
//
// Budget rationale: each capture costs one analyzer transcript scan plus one
// git tree write — empirically tens of milliseconds, about the same as one
// post-todo incremental checkpoint. Claude Code's Stop hook has a default
// 60s timeout, and the rest of turn-end (transcript flush wait, SaveStep)
// already consumes a variable share of that budget before this backstop
// runs. Capping at 8 bounds the added worst case to well under a second while
// covering any realistic number of concurrent background subagents; a task
// past the cap is simply delayed — it's picked up on a later turn-end (the
// rotation guarantees it eventually surfaces, not just the same 8 forever),
// or at SessionEnd, whichever comes first, so nothing is permanently missed.
//
// The SessionEnd final path (final == true) does NOT apply this cap: there is
// no "next turn-end" for it to defer to — SessionEnd is the last chance to
// capture a task's transcript before the marker's information is gone for
// good, so it processes every marker regardless of count. It isn't on the
// per-turn latency budget this cap protects, and each Final capture
// claims (completes) its own record (claimTaskRecord), so the live set it
// iterates only shrinks as it goes.
const maxInFlightTasksPerCapture = 8

// captureInFlightTasks is the turn-end/SessionEnd backstop for background
// subagents dispatched by sessionID that haven't yet received their
// authoritative SubagentStop capture (live session.TaskRecord entries — see
// recordInFlightTaskLaunch). It is best-effort throughout: a failure
// capturing one task is logged and does not stop the others or fail the
// caller, since this is a backstop for a signal (SubagentStop) that is still
// expected to arrive and finalize things properly.
//
//   - final == false (turn-end): snapshots each task's code changes so far via
//     an incremental checkpoint. The marker is left in place — the task is
//     still running, and SubagentStop remains the authoritative final
//     capture. See captureInFlightTaskIncremental.
//   - final == true (SessionEnd): the session is closing with these tasks
//     still in flight, so each gets the same non-incremental, transcript-
//     including capture SubagentStop would have performed, via the same
//     handleSubagentStopFinal machinery. See captureInFlightTaskFinal. This
//     MUST run before the caller marks the session ended and eagerly
//     condenses (endSessionNow): condensation sweeps whatever task steps
//     already exist on the shadow branch, so capturing after it would mint
//     shadow data nothing then condenses — the same zombie class
//     handleSubagentStopFinal's late-arrival guard exists to
//     prevent.
func captureInFlightTasks(ctx context.Context, ag agent.Agent, sessionID, sessionRef string, final bool) {
	logCtx := logging.WithAgent(logging.WithComponent(ctx, "lifecycle"), ag.Name())

	state, err := strategy.LoadSessionState(logCtx, sessionID)
	if err != nil {
		logging.Warn(logCtx, "failed to load session state for in-flight task capture",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()))
		return
	}
	if state == nil {
		return
	}
	liveTasks := state.LiveTaskRecords()
	if len(liveTasks) == 0 {
		return
	}

	// The cap applies only to the incremental (turn-end) path: a Final
	// (SessionEnd) capture is every task's last chance to keep its
	// transcript, so it must never be clipped — see maxInFlightTasksPerCapture.
	//
	// Selection is NOT a straight oldest-StartedAt prefix: TaskRecords is
	// append-only in launch order, so a plain prefix would always pick the
	// same first maxInFlightTasksPerCapture records by launch order, and any
	// task beyond the cap would never get an incremental snapshot for the
	// life of the session — contradicting the cap comment's promise that a
	// clipped task is "picked up next turn-end." selectInFlightTasksForSnapshot
	// instead rotates the selection by least-recently-attempted, and the
	// stamp below records this batch's attempt so the next turn-end picks a
	// different set.
	tasks := liveTasks
	if !final {
		if len(tasks) > maxInFlightTasksPerCapture {
			// Expected, self-healing: the rotation picks up the remainder on a
			// later turn-end, or SessionEnd's uncapped final capture does.
			logging.Info(logCtx, "clipping in-flight task capture to per-invocation budget",
				slog.String("session_id", sessionID),
				slog.Int("in_flight_count", len(tasks)),
				slog.Int("cap", maxInFlightTasksPerCapture))
		}
		tasks = selectInFlightTasksForSnapshot(tasks, maxInFlightTasksPerCapture)
		// Stamped before processing, in one batch mutation, regardless of each
		// task's eventual per-task outcome (skipped, deduped, or saved): the
		// stamp is what rotates the selection, so it must land even if a
		// later task in this same batch fails to capture.
		stampInFlightSnapshotAttempts(logCtx, sessionID, tasks)
	}

	for _, task := range tasks {
		if final {
			captureInFlightTaskFinal(logCtx, ag, sessionID, sessionRef, task)
		} else {
			captureInFlightTaskIncremental(logCtx, ag, sessionID, sessionRef, task)
		}
	}
}

// selectInFlightTasksForSnapshot picks up to maxCount markers for a single
// turn-end incremental-snapshot attempt, ordered by least-recently-attempted
// (zero LastSnapshotAttempt — never attempted — sorts first), ties broken by
// StartedAt. Returns tasks unmodified (no copy, no reorder) when it already
// fits within maxCount, so the common case (few concurrent background tasks)
// costs nothing.
//
// This is the fix for the starvation regression: TaskRecords is
// append-only in launch order, so a plain oldest-StartedAt prefix always
// selects the same first `maxCount` markers by launch order — any task
// beyond the cap would never receive an incremental snapshot for the life of
// the session, contradicting maxInFlightTasksPerCapture's promise that a
// clipped task is merely delayed to a later turn-end. Selecting by
// least-recently-attempted instead means every marker's turn comes up within
// ceil(len(tasks)/maxCount) turn-ends, however many are in flight.
func selectInFlightTasksForSnapshot(tasks []session.TaskRecord, maxCount int) []session.TaskRecord {
	if len(tasks) <= maxCount {
		return tasks
	}
	ordered := make([]session.TaskRecord, len(tasks))
	copy(ordered, tasks)
	slices.SortStableFunc(ordered, func(a, b session.TaskRecord) int {
		if !a.LastSnapshotAttempt.Equal(b.LastSnapshotAttempt) {
			if a.LastSnapshotAttempt.Before(b.LastSnapshotAttempt) {
				return -1
			}
			return 1
		}
		if a.StartedAt.Before(b.StartedAt) {
			return -1
		}
		if b.StartedAt.Before(a.StartedAt) {
			return 1
		}
		return 0
	})
	return ordered[:maxCount]
}

// stampInFlightSnapshotAttempts records now as LastSnapshotAttempt on every
// marker in the SELECTED batch, in one MutateSessionState call, regardless of
// each task's eventual per-task capture outcome (skipped, deduped, or saved).
// This stamp is what rotates selectInFlightTasksForSnapshot's next-turn-end
// choice away from this batch — without it, an unattempted marker would never
// stop sorting first and the rotation would never advance past the first
// selection. Best-effort: a failure here is logged and does not block the
// capture attempts about to run against the unmutated `tasks` slice.
func stampInFlightSnapshotAttempts(logCtx context.Context, sessionID string, tasks []session.TaskRecord) {
	if len(tasks) == 0 {
		return
	}
	selected := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		selected[task.ToolUseID] = struct{}{}
	}
	now := time.Now()
	mutErr := strategy.MutateSessionState(logCtx, sessionID, func(state *strategy.SessionState) error {
		for i := range state.TaskRecords {
			if _, ok := selected[state.TaskRecords[i].ToolUseID]; ok {
				state.TaskRecords[i].LastSnapshotAttempt = now
			}
		}
		return nil
	})
	switch {
	case errors.Is(mutErr, strategy.ErrStateNotFound):
		logging.Debug(logCtx, "session state gone before snapshot-attempt stamp; nothing to rotate",
			slog.String("session_id", sessionID))
	case mutErr != nil:
		logging.Warn(logCtx, "failed to stamp in-flight task snapshot attempt timestamps",
			slog.String("session_id", sessionID),
			slog.String("error", mutErr.Error()))
	}
}

// captureInFlightTaskFinal runs the SessionEnd sweep's final capture for a
// single in-flight task by synthesizing the same Event shape a real
// SubagentStop would carry (SubagentStop payloads carry no tool_input either,
// which is why the marker itself is the source for SubagentType/
// TaskDescription/AgentID here) and delegating to handleSubagentStopFinal —
// the exact machinery a genuine late SubagentStop would run, including the
// claimTaskRecord race guard against a real SubagentStop for the same task
// arriving around the same time.
func captureInFlightTaskFinal(logCtx context.Context, ag agent.Agent, sessionID, sessionRef string, task session.TaskRecord) {
	event := &agent.Event{
		Type:            agent.SubagentEnd,
		SessionID:       sessionID,
		SessionRef:      sessionRef,
		ToolUseID:       task.ToolUseID,
		SubagentID:      task.AgentID,
		SubagentType:    task.SubagentType,
		TaskDescription: task.TaskDescription,
		Final:           true,
		Timestamp:       time.Now(),
	}
	if err := handleSubagentStopFinal(logCtx, ag, event); err != nil {
		logging.Warn(logCtx, "failed to finalize in-flight task at session end",
			slog.String("session_id", sessionID),
			slog.String("tool_use_id", task.ToolUseID),
			slog.String("error", err.Error()))
	}
}

// captureInFlightTaskIncremental snapshots one in-flight task's code changes
// as an incremental checkpoint at turn-end. Deliberately hand-built rather
// than routed through captureSubagentTaskStep: that helper calls
// CleanupPreTaskState (on both success and its no-changes skip), which
// would destroy the pre-task untracked-files baseline on the very first
// incremental pass — before the task's eventual Final capture ever runs —
// making that Final capture misclassify every pre-existing untracked file as
// new. The marker also stays in place: this is a backstop snapshot, not a
// completion signal, so it must never remove the marker or clean up pre-task
// state; SubagentStop (or the SessionEnd final capture) owns both.
func captureInFlightTaskIncremental(logCtx context.Context, ag agent.Agent, sessionID, sessionRef string, task session.TaskRecord) {
	// Resolve the subagent's own transcript the same way captureSubagentTaskStep
	// does. Skip silently when it doesn't exist yet — a subagent that has
	// barely started may not have written its transcript file at all, and this
	// is just a backstop; the next turn-end (or SubagentStop) will catch it.
	subagentTranscriptPath := ResolveAgentTranscriptPath(filepath.Dir(sessionRef), sessionID, task.AgentID)
	if subagentTranscriptPath == "" {
		logging.Debug(logCtx, "in-flight task transcript not yet available; skipping turn-end snapshot",
			slog.String("session_id", sessionID),
			slog.String("tool_use_id", task.ToolUseID))
		return
	}

	// Growth dedup: skip the analyzer scan and shadow-branch commit entirely
	// when the transcript hasn't grown since the last scan that fully
	// accounted for it — whether or not that scan wrote a checkpoint (see
	// persistCapturedTranscriptSize). Without this, every turn-end after a task's last
	// real progress re-scans the whole transcript and writes a
	// content-identical checkpoint — a per-turn cost that grows with the
	// transcript and adds pure noise to the checkpoint history.
	info, statErr := os.Stat(subagentTranscriptPath)
	if statErr != nil {
		logging.Warn(logCtx, "failed to stat subagent transcript for in-flight task snapshot",
			slog.String("tool_use_id", task.ToolUseID),
			slog.String("error", statErr.Error()))
		return
	}
	transcriptSize := info.Size()
	if transcriptSize == task.LastCapturedTranscriptBytes {
		logging.Debug(logCtx, "subagent transcript unchanged since last snapshot; skipping incremental capture",
			slog.String("session_id", sessionID),
			slog.String("tool_use_id", task.ToolUseID),
			slog.Int64("transcript_bytes", transcriptSize))
		return
	}

	// Extract modified files from the SUBAGENT's own transcript — the same
	// analyzer call captureSubagentTaskStep uses — rather than from git status
	// directly: turn-end runs in the parent's working tree, where git status
	// reflects everything changed since the turn started, not just this one
	// background task's contribution.
	var modifiedFiles []string
	if analyzer, ok := agent.AsTranscriptAnalyzer(ag); ok {
		files, _, fileErr := analyzer.ExtractModifiedFilesFromOffset(subagentTranscriptPath, 0)
		if fileErr != nil {
			// Skip the save AND the size persistence: if we recorded
			// transcriptSize here, the growth dedup above would treat this size
			// as "fully accounted for" and skip retrying on every subsequent
			// turn-end, permanently losing this task's progress at this size.
			// Returning without persisting leaves LastCapturedTranscriptBytes at
			// its prior value, so the next turn-end retries the scan.
			logging.Warn(logCtx, "failed to extract modified files for in-flight task snapshot; will retry next turn-end",
				slog.String("session_id", sessionID),
				slog.String("tool_use_id", task.ToolUseID),
				slog.String("error", fileErr.Error()))
			return
		}
		modifiedFiles = files
	}

	repoRoot, err := paths.WorktreeRoot(logCtx)
	if err != nil {
		logging.Warn(logCtx, "failed to get worktree root for in-flight task snapshot",
			slog.String("tool_use_id", task.ToolUseID),
			slog.String("error", err.Error()))
		return
	}

	// Exclude files already committed to HEAD — the same guard
	// captureSubagentTaskStep applies, for the same reason: if the subagent
	// committed its own work mid-turn, that commit already condensed the
	// session, and re-adding those files here would resurrect content that's
	// already landed.
	relModifiedFiles := filterToUncommittedFiles(logCtx, FilterAndNormalizePaths(modifiedFiles, repoRoot), repoRoot)
	if len(relModifiedFiles) == 0 {
		logging.Debug(logCtx, "no file changes detected for in-flight task, skipping incremental snapshot",
			slog.String("tool_use_id", task.ToolUseID))
		// Still record the transcript size: a read-only background subagent
		// (e.g. a reviewer) never has file changes, so without this its
		// unchanged transcript would fail the growth-dedup check (which
		// compares against LastCapturedTranscriptBytes) and get rescanned by
		// the analyzer on every single turn-end for the life of the session.
		persistCapturedTranscriptSize(logCtx, sessionID, task.ToolUseID, transcriptSize)
		return
	}

	author, err := GetGitAuthor(logCtx)
	if err != nil {
		logging.Warn(logCtx, "failed to get git author for in-flight task snapshot",
			slog.String("tool_use_id", task.ToolUseID),
			slog.String("error", err.Error()))
		return
	}

	seq := GetNextCheckpointSequence(sessionID, task.ToolUseID)

	// SubagentTranscriptPath is deliberately omitted: the incremental write
	// path (checkpoint/ephemeral.go's IsIncremental branch) never stores a
	// transcript, so setting it here would imply storage that doesn't happen.
	taskStepCtx := strategy.TaskStepContext{
		SessionID:           sessionID,
		ToolUseID:           task.ToolUseID,
		AgentID:             task.AgentID,
		ModifiedFiles:       relModifiedFiles,
		AuthorName:          author.Name,
		AuthorEmail:         author.Email,
		IsIncremental:       true,
		IncrementalSequence: seq,
		IncrementalType:     strategy.IncrementalTypeBackgroundProgress,
		SubagentType:        task.SubagentType,
		TaskDescription:     task.TaskDescription,
		AgentType:           ag.Type(),
	}

	if err := GetStrategy(logCtx).SaveTaskStep(logCtx, taskStepCtx); err != nil {
		logging.Warn(logCtx, "failed to save incremental snapshot for in-flight task",
			slog.String("tool_use_id", task.ToolUseID),
			slog.String("error", err.Error()))
		return
	}

	persistCapturedTranscriptSize(logCtx, sessionID, task.ToolUseID, transcriptSize)
}

// persistCapturedTranscriptSize records a subagent transcript's just-scanned
// size on its in-flight marker so a subsequent turn-end with an unchanged
// transcript can skip both the analyzer scan and any shadow-branch write (the
// growth dedup in captureInFlightTaskIncremental). Called from both the
// zero-files skip path and after a successful incremental save — either way,
// the transcript at this size has now been fully accounted for.
//
// The record itself is no longer removed once created — CompleteTaskRecord
// marks it completed rather than deleting it — so marker == nil here would
// mean the whole session state vanished between the earlier LoadSessionState
// in captureInFlightTasks and now, or (nothing currently does this) the
// record was explicitly removed; tolerated the same way the rest of this
// path tolerates a vanished marker (nothing to update). A Final capture
// racing this same call (claimTaskRecord) instead marks the SAME record
// completed, so this write can land on an already-completed record — harmless
// but pointless (this incremental-only field dies along with the rest of the
// turn-end backstop in Task 4), so skip the write once completion has landed.
func persistCapturedTranscriptSize(logCtx context.Context, sessionID, toolUseID string, transcriptSize int64) {
	mutErr := strategy.MutateSessionState(logCtx, sessionID, func(state *strategy.SessionState) error {
		marker := state.FindTaskRecord(toolUseID)
		if marker == nil || !marker.CompletedAt.IsZero() {
			return nil
		}
		marker.LastCapturedTranscriptBytes = transcriptSize
		return nil
	})
	if mutErr != nil && !errors.Is(mutErr, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to persist captured transcript size for in-flight task",
			slog.String("tool_use_id", toolUseID),
			slog.String("error", mutErr.Error()))
	}
}

// resolveSubagentSessionLink reports whether this turn belongs to a subagent
// session spawned by a parent task invocation. It fails closed: an agent that
// does not model detached subagent sessions, or a link that cannot be read,
// leaves the turn on the ordinary session-checkpoint path.
func resolveSubagentSessionLink(
	ctx context.Context,
	ag agent.Agent,
	transcriptRef string,
) (agent.SubagentSessionLink, bool) {
	resolver, ok := agent.AsSubagentSessionResolver(ag)
	if !ok {
		return agent.SubagentSessionLink{}, false
	}
	link, isSubagent := resolver.ResolveSubagentSession(transcriptRef)
	if !isSubagent {
		return agent.SubagentSessionLink{}, false
	}
	// Both IDs are interpolated into metadata paths by
	// SessionMetadataDirFromSessionID and TaskMetadataDir, neither of which
	// sanitizes. Enforce it here rather than trusting each implementation: this
	// is the one choke point every SubagentSessionResolver passes through, and
	// it mirrors the ValidateSessionID checks the hook dispatcher already
	// applies to IDs arriving from an agent.
	logCtx := logging.WithComponent(ctx, "lifecycle")
	if err := validation.ValidateAgentSessionID(link.ParentSessionID); err != nil {
		logging.Warn(logCtx, "ignoring subagent session link with invalid parent session ID",
			slog.String("error", err.Error()))
		return agent.SubagentSessionLink{}, false
	}
	if err := validation.ValidateToolUseID(link.ToolUseID); err != nil {
		logging.Warn(logCtx, "ignoring subagent session link with invalid tool use ID",
			slog.String("error", err.Error()))
		return agent.SubagentSessionLink{}, false
	}
	// An empty tool-use ID passes ValidateToolUseID (the field is optional
	// elsewhere) but cannot name a task directory here.
	if link.ToolUseID == "" {
		logging.Warn(logCtx, "ignoring subagent session link with empty tool use ID")
		return agent.SubagentSessionLink{}, false
	}
	logging.Debug(logCtx, "resolved subagent session",
		slog.String("parent_session_id", link.ParentSessionID),
		slog.String("tool_use_id", link.ToolUseID),
		slog.String("subagent_type", link.SubagentType))
	return link, true
}

// subagentSessionStep carries the turn-end inputs needed to record a detached
// subagent session as a task checkpoint on its parent.
type subagentSessionStep struct {
	link          agent.SubagentSessionLink
	sessionID     string
	event         *agent.Event
	transcriptRef string
	modifiedFiles []string
	newFiles      []string
	deletedFiles  []string
	author        *GitAuthor
	agentType     types.AgentType
	strat         *strategy.ManualCommitStrategy
}

// saveSubagentSessionTaskStep writes a subagent session's turn as a task
// checkpoint under its parent session.
//
// The checkpoint is keyed by the parent's session and tool-use ID, so the
// subagent's files land in the parent's FilesTouched and its work condenses with
// the parent's next commit. The subagent's own session ID doubles as the agent
// ID, which is what stores its transcript beside the parent's in the checkpoint.
func saveSubagentSessionTaskStep(ctx context.Context, step subagentSessionStep) error {
	logCtx := logging.WithComponent(ctx, "lifecycle")
	logging.Info(logCtx, "recording subagent session as task checkpoint",
		slog.String("subagent_session_id", step.sessionID),
		slog.String("parent_session_id", step.link.ParentSessionID),
		slog.String("tool_use_id", step.link.ToolUseID),
		slog.Int("modified_files", len(step.modifiedFiles)),
		slog.Int("new_files", len(step.newFiles)),
		slog.Int("deleted_files", len(step.deletedFiles)))

	taskStepCtx := strategy.TaskStepContext{
		SessionID:              step.link.ParentSessionID,
		ToolUseID:              step.link.ToolUseID,
		AgentID:                step.sessionID,
		ModifiedFiles:          step.modifiedFiles,
		NewFiles:               step.newFiles,
		DeletedFiles:           step.deletedFiles,
		TranscriptPath:         step.link.ParentTranscriptPath,
		SubagentTranscriptPath: step.transcriptRef,
		AuthorName:             step.author.Name,
		AuthorEmail:            step.author.Email,
		SubagentType:           step.link.SubagentType,
		TaskDescription:        step.link.TaskDescription,
		AgentType:              step.agentType,
	}

	if err := step.strat.SaveTaskStep(ctx, taskStepCtx); err != nil {
		return fmt.Errorf("failed to save subagent session task step: %w", err)
	}

	// Retire the subagent's own session the same way an ordinary turn-end does,
	// so its phase and pre-prompt state do not linger as an active session.
	transitionSessionTurnEnd(ctx, step.sessionID, step.event)
	if cleanupErr := CleanupPrePromptState(ctx, step.sessionID); cleanupErr != nil {
		logging.Warn(logCtx, "failed to cleanup pre-prompt state",
			slog.String("error", cleanupErr.Error()))
	}
	return nil
}

// --- Helper functions ---

// resolveTranscriptOffset determines the transcript offset to use for parsing.
// Prefers pre-prompt state, falls back to session state.
func resolveTranscriptOffset(ctx context.Context, preState *PrePromptState, sessionID string) int {
	logCtx := logging.WithComponent(ctx, "lifecycle")
	if preState != nil && preState.TranscriptOffset > 0 {
		logging.Debug(logCtx, "pre-prompt state found, parsing transcript from offset",
			slog.Int("offset", preState.TranscriptOffset))
		return preState.TranscriptOffset
	}

	// Fall back to session state
	sessionState, loadErr := strategy.LoadSessionState(ctx, sessionID)
	if loadErr != nil {
		logging.Warn(logCtx, "failed to load session state",
			slog.String("error", loadErr.Error()))
		return 0
	}
	if sessionState != nil && sessionState.CheckpointTranscriptStart > 0 {
		logging.Debug(logCtx, "session state found, parsing transcript from offset",
			slog.Int("offset", sessionState.CheckpointTranscriptStart))
		return sessionState.CheckpointTranscriptStart
	}

	return 0
}

// parseTranscriptForCheckpointUUID is a thin wrapper around transcript parsing for checkpoint UUID lookup.
// Returns parsed transcript lines for use with FindCheckpointUUID.
func parseTranscriptForCheckpointUUID(transcriptPath string) ([]transcriptLine, error) {
	lines, err := transcript.ParseFromFileAtLine(transcriptPath, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing transcript for checkpoint UUID: %w", err)
	}
	return lines, nil
}

// transitionSessionTurnEnd transitions the session phase to IDLE and dispatches turn-end actions.
func transitionSessionTurnEnd(ctx context.Context, sessionID string, event *agent.Event) {
	logCtx := logging.WithComponent(ctx, "lifecycle")
	mutErr := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
		persistEventMetadataToState(event, state)
		if err := strategy.TransitionAndLog(ctx, state, session.EventTurnEnd, session.TransitionContext{}, session.NoOpActionHandler{}); err != nil {
			logging.Warn(logCtx, "turn-end transition failed",
				slog.String("error", err.Error()))
		}
		// HandleTurnEnd mutates state in-place; the outer MutateSessionState
		// save flushes those changes. Any reentrant MutateSessionState calls
		// it makes on this session ID share this state pointer via the gate.
		strat := GetStrategy(ctx)
		if err := strat.HandleTurnEnd(ctx, state); err != nil {
			logging.Warn(logCtx, "turn-end action dispatch failed",
				slog.String("error", err.Error()))
		}
		return nil
	})
	if mutErr != nil && !errors.Is(mutErr, strategy.ErrStateNotFound) {
		logging.Warn(logCtx, "failed to update session phase on turn end",
			slog.String("error", mutErr.Error()))
	}
}

// sessionEndedAt resolves the EndedAt stamp for a session being finalized under
// the given policy. endedWhenLastSeen falls back through the state's own record
// of activity and never yields a zero time: an unknown last-seen is stamped now,
// which is what the old unconditional behavior did anyway.
func sessionEndedAt(state *strategy.SessionState, when endedAtPolicy) time.Time {
	if when == endedWhenLastSeen {
		if state.LastInteractionTime != nil && !state.LastInteractionTime.IsZero() {
			return *state.LastInteractionTime
		}
		if !state.StartedAt.IsZero() {
			return state.StartedAt
		}
	}
	return time.Now()
}

// endedAtPolicy selects the timestamp written to SessionState.EndedAt.
type endedAtPolicy int

const (
	// endedNow stamps the current time: the session is ending as we watch it,
	// driven by its own session-end hook or by `entire session stop`.
	endedNow endedAtPolicy = iota

	// endedWhenLastSeen stamps the session's last known activity instead, for
	// finalizations that discover an end that already happened. The exited-owner
	// sweep is the case: the agent quit at some unknown earlier point, and since
	// the sweep covers IDLE and state files live for StaleSessionThreshold, the
	// first run after an upgrade can finalize sessions abandoned days ago.
	//
	// Stamping "now" on those dates a week-old session to today, which floats it
	// above genuinely recent work in the `entire session resume` picker
	// (sessionLastActiveTime prefers EndedAt) and makes `entire session info`
	// report it as just-ended. Only display and ordering read the value — nothing
	// keys retention off it — so the older, truer timestamp is strictly better.
	endedWhenLastSeen
)

// markSessionEnded transitions the session to ENDED phase via the state machine.
// If event is non-nil, hook-provided metrics are persisted to state before saving.
// markSessionEnded fires the SessionStop transition (PhaseEnded + EndedAt) under
// the session-state lock. When guard is non-nil and returns false on the
// freshly-loaded state, the transition is skipped — callers use it to
// re-validate a precondition that may have changed since their snapshot (the
// exited-session sweep re-checks OwnerExited under the lock so it never ends a
// session a concurrent turn just revived). It reports whether the session was
// actually ended.
func markSessionEnded(ctx context.Context, event *agent.Event, sessionID string, guard func(*strategy.SessionState) bool, when endedAtPolicy) (ended bool, err error) {
	mutErr := strategy.MutateSessionState(ctx, sessionID, func(state *strategy.SessionState) error {
		if guard != nil && !guard(state) {
			return strategy.ErrMutationSkip
		}
		if event != nil {
			persistEventMetadataToState(event, state)
		}
		// Resolved before the transition, which is not a read-only step: the
		// SessionStop edge carries ActionUpdateLastInteraction and stamps
		// LastInteractionTime with now — exactly the value endedWhenLastSeen
		// needs, so reading it afterwards always yields "now".
		endedAt := sessionEndedAt(state, when)
		if transErr := strategy.TransitionAndLog(ctx, state, session.EventSessionStop, session.TransitionContext{}, session.NoOpActionHandler{}); transErr != nil {
			logging.Warn(logging.WithComponent(ctx, "lifecycle"), "session stop transition failed",
				slog.String("error", transErr.Error()))
		}
		state.EndedAt = &endedAt
		ended = true
		return nil
	})
	if errors.Is(mutErr, strategy.ErrStateNotFound) || errors.Is(mutErr, strategy.ErrMutationSkip) {
		return false, nil
	}
	if mutErr != nil {
		return false, fmt.Errorf("failed to save session state: %w", mutErr)
	}
	return ended, nil
}

// logFileChanges logs the files modified, created, and deleted during a session.
func logFileChanges(ctx context.Context, modified, newFiles, deleted []string) {
	logCtx := logging.WithComponent(ctx, "lifecycle")
	logging.Debug(logCtx, "files changed during session",
		slog.Int("modified", len(modified)),
		slog.Int("new", len(newFiles)),
		slog.Int("deleted", len(deleted)))
}

func persistEventMetadataToState(event *agent.Event, state *strategy.SessionState) {
	// Update ModelName if provided (model is known by turn-end even on first turn)
	if event.Model != "" {
		state.ModelName = event.Model
	}
	appendEventSkillEventsToState(event, state)

	// Persist hook-provided session metrics (e.g., from Cursor hooks)
	if event.DurationMs > 0 {
		state.SessionDurationMs = event.DurationMs
	}
	// Use hook-reported turn count if available (take max); otherwise
	// increment on each TurnEnd event to count turns ourselves.
	prevTurnCount := state.SessionTurnCount
	if event.TurnCount > 0 {
		if event.TurnCount > state.SessionTurnCount {
			state.SessionTurnCount = event.TurnCount
		}
	} else if event.Type == agent.TurnEnd {
		state.SessionTurnCount++
	}
	// Deferred checkpoint-window reset: the first time the turn count actually
	// advances after a checkpoint was written, re-anchor the window base to the
	// count from before this turn so the current turn becomes the first prompt of
	// the new window. Gate on a real advance (not just a TurnEnd / non-zero
	// TurnCount) so a repeated or stale hook reporting the same cumulative count
	// doesn't re-anchor early — that would make a later back-to-back checkpoint
	// report 1 instead of matching the prior count.
	if state.SessionTurnCount > prevTurnCount && state.PromptWindowResetPending {
		state.PromptWindowBase = prevTurnCount
		state.PromptWindowResetPending = false
	}
	if event.ContextTokens > 0 {
		state.ContextTokens = event.ContextTokens
	}
	if event.ContextWindowSize > 0 {
		state.ContextWindowSize = event.ContextWindowSize
	}
}

func appendEventSkillEventsToState(event *agent.Event, state *strategy.SessionState) bool {
	if event == nil || state == nil || len(event.SkillEvents) == 0 {
		return false
	}
	changed := false
	for _, skillEvent := range event.SkillEvents {
		if skillEvent.TurnID == "" {
			skillEvent.TurnID = state.TurnID
		}
		if skillEventExists(state.SkillEvents, skillEvent) {
			continue
		}
		state.SkillEvents = append(state.SkillEvents, skillEvent)
		changed = true
	}
	return changed
}

func skillEventExists(events []agent.SkillEvent, candidate agent.SkillEvent) bool {
	for _, existing := range events {
		if existing.ID != "" && candidate.ID != "" {
			if existing.ID == candidate.ID {
				return true
			}
			continue
		}
		if existing.EventType == candidate.EventType &&
			existing.Skill.Name == candidate.Skill.Name &&
			existing.Source.Agent == candidate.Source.Agent &&
			existing.Source.Signal == candidate.Source.Signal &&
			existing.TurnID == candidate.TurnID {
			return true
		}
	}
	return false
}

// envAdoptionSpec carries the kind-specific bits of env-driven session
// tagging. The shared scaffolding (idempotence guard, SESSION/AGENT/
// STARTING_SHA gates) lives in tryAdoptEnv; apply runs only after the gates
// pass and is responsible for decoding the kind-specific payload, mutating
// state.Kind and the related fields, and emitting the success log.
type envAdoptionSpec struct {
	kindLabel      string // "review" or "investigate" — log prefix
	envSession     string
	envAgent       string
	envStartingSHA string
	apply          func(ctx context.Context, state *session.State, expectedAgent string)
}

// tryAdoptEnv runs the shared env-adoption protocol for a launched-agent
// process and delegates kind-specific decode/apply to spec.apply.
//
// The protocol:
//  1. If state.Kind is already set, do nothing — adoption is idempotent
//     across turns, and a session is review OR investigate, not both.
//  2. envSession must be "1". `entire review` / `entire investigate` set
//     this on the spawned agent process; the lifecycle hook (a child of
//     the agent) inherits it naturally.
//  3. envAgent must match the hook's agent — protects against stale env
//     vars inherited from a parent shell or a nested invocation.
//  4. envStartingSHA must match the session's BaseCommit — protects
//     against env vars surviving a commit boundary.
//
// All failures log at debug/warn and leave state untagged.
//
// Trust model: this gate (env-present + agent-match + SHA-match) treats
// the parent process environment as trusted. The CLI never exports these
// vars to a user shell — they exist only on the in-process env of agents
// spawned by `entire review` / `entire investigate` themselves, plus the
// lifecycle hook (a child of that agent) which inherits them naturally.
// A user who manually `export`s ENTIRE_REVIEW_AGENT=<their-agent> and
// ENTIRE_REVIEW_STARTING_SHA=<HEAD-sha> before launching an agent COULD
// forge a review-tagged session; that is considered out-of-scope for the
// adoption guard. The SHA gate also self-invalidates on the next commit
// (BaseCommit changes), so a stale-env forgery cannot persist across a
// commit boundary even if it succeeded once.
func tryAdoptEnv(ctx context.Context, state *session.State, expectedAgent string, spec envAdoptionSpec) {
	if state.Kind != "" {
		return
	}
	if envSession := os.Getenv(spec.envSession); envSession != "1" {
		logging.Debug(ctx, spec.kindLabel+" env adoption skipped: "+spec.envSession+" is not \"1\"",
			slog.String("expected_agent", expectedAgent),
			slog.String("observed_value", envSession))
		return
	}
	envAgent := os.Getenv(spec.envAgent)
	if envAgent != expectedAgent {
		logging.Warn(ctx, spec.kindLabel+" env adoption skipped: agent mismatch",
			slog.String("env_agent", envAgent),
			slog.String("hook_agent", expectedAgent))
		return
	}
	startingSHA := os.Getenv(spec.envStartingSHA)
	if startingSHA == "" || state.BaseCommit == "" || startingSHA != state.BaseCommit {
		logging.Warn(ctx, spec.kindLabel+" env adoption skipped: starting SHA mismatch",
			slog.String("env_starting_sha", startingSHA),
			slog.String("state_base_commit", state.BaseCommit))
		return
	}
	spec.apply(ctx, state, envAgent)
}

// adoptReviewEnv tags the session as a review session when ENTIRE_REVIEW_*
// env vars are present on the current process.
func adoptReviewEnv(ctx context.Context, state *session.State, expectedAgent string) {
	tryAdoptEnv(ctx, state, expectedAgent, envAdoptionSpec{
		kindLabel:      "review",
		envSession:     review.EnvSession,
		envAgent:       review.EnvAgent,
		envStartingSHA: review.EnvStartingSHA,
		apply: func(ctx context.Context, state *session.State, envAgent string) {
			skills, err := review.DecodeSkills(os.Getenv(review.EnvSkills))
			if err != nil {
				logging.Warn(ctx, "review env adoption failed: invalid skills JSON",
					slog.String("err", err.Error()))
				return
			}
			state.Kind = session.KindAgentReview
			state.ReviewSkills = skills
			state.ReviewPrompt = os.Getenv(review.EnvPrompt)
			logging.Debug(ctx, "adopted review env",
				slog.String("agent", envAgent),
				slog.Int("skill_count", len(skills)))
		},
	})
}

// adoptInvestigateEnv tags the session as an investigation session when
// ENTIRE_INVESTIGATE_* env vars are present on the current process.
//
// Adoption ordering: adoptReviewEnv runs first; if both env families are
// somehow set on the same process, review wins. Production strips
// ENTIRE_REVIEW_* in AppendInvestigateEnv before spawning each per-turn
// agent process, so this conflict cannot happen for fresh investigate spawns
// — but tryAdoptEnv's short-circuit on state.Kind != "" makes the conflict
// harmless if it ever arises.
func adoptInvestigateEnv(ctx context.Context, state *session.State, expectedAgent string) {
	tryAdoptEnv(ctx, state, expectedAgent, envAdoptionSpec{
		kindLabel:      "investigate",
		envSession:     provenance.InvestigateSession,
		envAgent:       provenance.InvestigateAgent,
		envStartingSHA: provenance.InvestigateStartingSHA,
		apply: func(ctx context.Context, state *session.State, envAgent string) {
			runID := os.Getenv(provenance.InvestigateRunID)
			// Reject empty or malformed RunID — downstream condensation joins
			// session metadata by run ID, and tagging a session with no/invalid
			// ID would leak into checkpoint metadata as junk data.
			if !provenance.IsValidRunID(runID) {
				logging.Warn(ctx, "investigate env adoption skipped: invalid run id",
					slog.String("env_run_id", runID))
				return
			}
			state.Kind = session.KindAgentInvestigate
			state.InvestigateRunID = runID
			state.InvestigateTopic = os.Getenv(provenance.InvestigateTopic)
			logging.Debug(ctx, "adopted investigate env",
				slog.String("agent", envAgent),
				slog.String("run_id", state.InvestigateRunID))
		},
	})
}
