package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/external"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/strategy"

	// Import agents to register them
	_ "github.com/entireio/cli/cmd/entire/cli/agent/claudecode"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/codex"
	_ "github.com/entireio/cli/cmd/entire/cli/agent/factoryaidroid"
)

// Package-level aliases to avoid shadowing the settings package with local variables named "settings".
const (
	EntireSettingsFile      = settings.EntireSettingsFile
	EntireSettingsLocalFile = settings.EntireSettingsLocalFile
)

// EntireSettings is an alias for settings.EntireSettings.
type EntireSettings = settings.EntireSettings

// LoadEntireSettings loads the Entire settings from .entire/settings.json,
// then applies any overrides from .entire/settings.local.json if it exists.
// Returns default settings if neither file exists.
// Works correctly from any subdirectory within the repository.
func LoadEntireSettings(ctx context.Context) (*settings.EntireSettings, error) {
	s, err := settings.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}
	return s, nil
}

// SaveEntireSettings saves the Entire settings to .entire/settings.json.
func SaveEntireSettings(ctx context.Context, s *settings.EntireSettings) error {
	if err := settings.Save(ctx, s); err != nil {
		return fmt.Errorf("saving settings: %w", err)
	}
	return nil
}

// SaveEntireSettingsLocal saves the Entire settings to .entire/settings.local.json.
func SaveEntireSettingsLocal(ctx context.Context, s *settings.EntireSettings) error {
	if err := settings.SaveLocal(ctx, s); err != nil {
		return fmt.Errorf("saving local settings: %w", err)
	}
	return nil
}

// IsEnabled returns whether Entire is currently enabled.
// Returns true by default if settings cannot be loaded.
func IsEnabled(ctx context.Context) (bool, error) {
	s, err := settings.Load(ctx)
	if err != nil {
		return true, err //nolint:wrapcheck // already present in codebase
	}
	return s.Enabled, nil
}

// GetStrategy returns the manual-commit strategy instance with blob fetching
// enabled so that checkpoint reads work after treeless fetches.
func GetStrategy(_ context.Context) *strategy.ManualCommitStrategy {
	s := strategy.NewManualCommitStrategy()
	s.SetBlobFetcher(FetchBlobsByHash)
	return s
}

// GetLogLevel returns the configured log level from settings.
// Returns empty string if not configured (caller should use default).
// Note: ENTIRE_LOG_LEVEL env var takes precedence; check it first.
func GetLogLevel() string {
	s, err := settings.Load(context.TODO()) //nolint:contextcheck // Called as a callback via SetLogLevelGetter, no ctx available
	if err != nil {
		return ""
	}
	return s.LogLevel
}

// ensureCommandLogging routes logging to .entire/logs/ for a plain command that
// would otherwise emit logging.* calls with no logger installed, and returns the
// teardown. Commands reached from a hook already have one, and this leaves it
// alone; see logging.EnsureInitialized.
//
// The level getter is paired with the init here — the same pairing every
// command-level Init site needs — because without it `log_level` from settings
// is ignored on this path.
//
// Scope the returned cleanup to the whole command, not to the one call that
// needed a logger: it closes the log file, so anything logged afterwards goes
// back to the user's terminal via slog.Default().
func ensureCommandLogging(ctx context.Context) func() {
	logging.SetLogLevelGetter(GetLogLevel)
	return logging.EnsureInitialized(ctx)
}

// agentHookState is the result of one hooks-installed sweep. Detecting is a
// subprocess per external plugin, so a caller needing both answers takes them
// from a single sweep rather than probing twice.
type agentHookState struct {
	// installed are the agents that reported hooks installed.
	installed []types.AgentName
	// unchecked are the agents that could not answer. Their hooks may or may not
	// be on disk, which is not the same as cleanly reporting none.
	unchecked []uncheckedAgent
}

// uncheckedAgent is an agent that could not be asked, with the reason. The
// reason travels with it because the sweep is the only place it exists: asking
// an external plugin again would cost another subprocess.
type uncheckedAgent struct {
	name types.AgentName
	err  error
	// external distinguishes the two remedies. A plugin owns its own hooks, so
	// only its binary can remove them and the user may have to run it by hand; a
	// built-in's hooks are removed in-process regardless of what its check said.
	external bool
}

// uncheckedExternal returns the plugins among the unchecked ones. Only they can
// leave hooks behind: a built-in is asked to uninstall regardless of what its
// check said, so nothing survives for the user to clean up by hand.
func (s agentHookState) uncheckedExternal() []uncheckedAgent {
	var plugins []uncheckedAgent
	for _, u := range s.unchecked {
		if u.external {
			plugins = append(plugins, u)
		}
	}
	return plugins
}

// names returns the unchecked agents' registry names, for display.
func (s agentHookState) uncheckedNames() []types.AgentName {
	names := make([]types.AgentName, 0, len(s.unchecked))
	for _, u := range s.unchecked {
		names = append(names, u.name)
	}
	return names
}

// getAgentHookState probes every hook-supporting agent exactly once, keeping a
// plugin that could not answer distinct from one reporting no hooks.
//
// A binary that fails `info` is never registered, so "unchecked" only ever
// covers a plugin that introduced itself and then failed to answer this.
func getAgentHookState(ctx context.Context) agentHookState {
	var state agentHookState
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		hs, ok := agent.AsHookSupport(ag)
		if !ok {
			continue
		}
		installed, err := hs.AreHooksInstalled(ctx)
		switch {
		case err == nil && installed:
			state.installed = append(state.installed, name)
		case err == nil:
			// Cleanly reported no hooks.
		case ctx.Err() != nil:
			// The context died, not the agent. Blaming every plugin on $PATH for
			// our own cancellation would turn one Ctrl-C into a page of diagnoses.
			logging.Debug(ctx, "hooks-installed check abandoned: context ended",
				"agent", string(name))
		default:
			// Built-in or plugin: if we have a reason, the caller can say so. A
			// broken .cursor/hooks.json is as worth reporting as a plugin that
			// crashed — what differs is the remedy, not whether to mention it.
			state.unchecked = append(state.unchecked, uncheckedAgent{
				name:     name,
				err:      err,
				external: external.IsExternal(ag),
			})
		}
	}
	return state
}

// GetAgentsWithHooksInstalled returns names of agents that have hooks installed.
// An agent that could not be asked is absent; callers that must act on that
// difference use getAgentHookState.
func GetAgentsWithHooksInstalled(ctx context.Context) []types.AgentName {
	return getAgentHookState(ctx).installed
}

// InstalledAgentDisplayNames returns user-facing display names for agents with hooks installed.
func InstalledAgentDisplayNames(ctx context.Context) []string {
	return agentDisplayNames(GetAgentsWithHooksInstalled(ctx))
}

// OutdatedHookAgents returns installed agents whose Entire hook config has
// drifted from what the CLI would write today, for `entire status` and
// `entire doctor` to surface. Agents that don't implement agent.HookFreshness
// are skipped: absence of a drift check reads as "nothing to report", never as
// a warning.
//
// Scoped to agents AreHooksInstalled reports as installed here. Note what that
// means for generated-file agents (Pi, OpenCode): the committed file *is* the
// installation, so a repo that ships one gets drift warnings even where nobody
// ran `entire agent add`. That is the intent — such a repo is relying on the
// committed file to work — but it does mean this is not scoped to people who
// opted in on this machine.
func OutdatedHookAgents(ctx context.Context) []types.AgentName {
	var outdated []types.AgentName
	for _, name := range GetAgentsWithHooksInstalled(ctx) {
		ag, err := agent.Get(name)
		if err != nil {
			continue
		}
		if hf, ok := agent.AsHookFreshness(ag); ok && hf.CheckHookConfig(ctx) == agent.HooksOutdated {
			outdated = append(outdated, name)
		}
	}
	return outdated
}

// OutdatedHookAgentDisplayNames returns user-facing display names for agents
// whose hook config is out of date.
func OutdatedHookAgentDisplayNames(ctx context.Context) []string {
	return agentDisplayNames(OutdatedHookAgents(ctx))
}

// agentDisplayNames maps agent names to their user-facing display names,
// skipping names that aren't registered.
func agentDisplayNames(names []types.AgentName) []string {
	displayNames := make([]string, 0, len(names))
	for _, name := range names {
		displayNames = append(displayNames, agentDisplayName(name))
	}
	return displayNames
}

// agentDisplayName returns one agent's user-facing name, falling back to the
// registry name when it cannot be looked up. Prose names an agent this way;
// only a command line the user is meant to run keeps the registry name, which
// is what the binary is called.
//
// The fallback matters because these names are how the user learns what a
// command is about to touch, or has left behind. Dropping an unresolvable name
// would list one fewer agent than will be acted on, and an empty one renders as
// a gap in the sentence.
func agentDisplayName(name types.AgentName) string {
	ag, err := agent.Get(name)
	if err != nil {
		return string(name)
	}
	return string(ag.Type())
}

// JoinAgentNames joins agent names into a comma-separated string.
func JoinAgentNames(names []types.AgentName) string {
	strs := make([]string, len(names))
	for i, n := range names {
		strs[i] = string(n)
	}
	return strings.Join(strs, ",")
}
