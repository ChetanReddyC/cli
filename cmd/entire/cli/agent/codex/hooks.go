package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// HooksFileName is the hooks config file used by Codex.
const HooksFileName = "hooks.json"

// entireHookPrefixes identifies Entire hook commands. The "go run" prefix is
// retained so hooks installed by older versions are still recognized.
var entireHookPrefixes = []string{
	"entire ",
	agent.LocalDevHookScript + " ",
	`go run "$(git rev-parse --show-toplevel)"/cmd/entire/main.go `,
}

// InstallHooks installs Codex hooks in .codex/hooks.json.
func (c *CodexAgent) InstallHooks(ctx context.Context, localDev bool, force bool) (int, error) {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot, err = os.Getwd() //nolint:forbidigo // Intentional fallback when WorktreeRoot() fails (tests)
		if err != nil {
			return 0, fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)

	// Read existing hooks.json if present
	var rawHooks map[string]json.RawMessage
	existingData, readErr := os.ReadFile(hooksPath) //nolint:gosec // path constructed from repo root
	if readErr == nil {
		var hooksFile map[string]json.RawMessage
		if err := json.Unmarshal(existingData, &hooksFile); err != nil {
			return 0, fmt.Errorf("failed to parse existing hooks.json: %w", err)
		}
		if hooksRaw, ok := hooksFile["hooks"]; ok {
			if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
				return 0, fmt.Errorf("failed to parse hooks in hooks.json: %w", err)
			}
		}
	}

	if rawHooks == nil {
		rawHooks = make(map[string]json.RawMessage)
	}

	// Parse event types we manage
	var sessionStart, userPromptSubmit, stop, postToolUse []MatcherGroup
	if err := parseHookType(rawHooks, "SessionStart", &sessionStart); err != nil {
		return 0, err
	}
	if err := parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit); err != nil {
		return 0, err
	}
	if err := parseHookType(rawHooks, "Stop", &stop); err != nil {
		return 0, err
	}
	if err := parseHookType(rawHooks, "PostToolUse", &postToolUse); err != nil {
		return 0, err
	}

	if force {
		sessionStart = removeEntireHooks(sessionStart)
		userPromptSubmit = removeEntireHooks(userPromptSubmit)
		stop = removeEntireHooks(stop)
		postToolUse = removeEntireHooks(postToolUse)
	}

	// Build hook commands
	var cmdPrefix string
	if localDev {
		cmdPrefix = agent.LocalDevHookScript + " hooks codex "
	} else {
		cmdPrefix = "entire hooks codex "
	}
	sessionStartCmd := cmdPrefix + "session-start"
	useWindowsProductionHooks := agent.UseWindowsProductionHooks(ctx, localDev)
	if !localDev {
		sessionStartCmd = agent.WrapProductionJSONWarningHookCommandForOS(sessionStartCmd, agent.WarningFormatSingleLine, useWindowsProductionHooks)
	}
	userPromptSubmitCmd := cmdPrefix + "user-prompt-submit"
	stopCmd := cmdPrefix + "stop"
	postToolUseCmd := cmdPrefix + "post-tool-use"
	if !localDev {
		userPromptSubmitCmd = agent.WrapProductionSilentHookCommandForOS(userPromptSubmitCmd, useWindowsProductionHooks)
		stopCmd = agent.WrapProductionSilentHookCommandForOS(stopCmd, useWindowsProductionHooks)
		postToolUseCmd = agent.WrapProductionSilentHookCommandForOS(postToolUseCmd, useWindowsProductionHooks)
	}

	count := 0

	if updated, changed := syncHookCommand(sessionStart, sessionStartCmd); changed {
		sessionStart = updated
		count++
	}
	if updated, changed := syncHookCommand(userPromptSubmit, userPromptSubmitCmd); changed {
		userPromptSubmit = updated
		count++
	}
	if updated, changed := syncHookCommand(stop, stopCmd); changed {
		stop = updated
		count++
	}
	if updated, changed := syncHookCommand(postToolUse, postToolUseCmd); changed {
		postToolUse = updated
		count++
	}

	if count == 0 {
		// Still self-heal a stale feature-flag config.toml left by an
		// older entire version, even if hooks were already present.
		if err := cleanupStaleFeatureConfig(repoRoot); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// Marshal modified types back
	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	// Preserve existing top-level keys (e.g., $schema) by reusing the parsed file
	topLevel := make(map[string]json.RawMessage)
	if readErr == nil {
		// Re-parse the original file to preserve all top-level keys
		_ = json.Unmarshal(existingData, &topLevel) //nolint:errcheck // best-effort preservation
	}
	hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks: %w", err)
	}
	topLevel["hooks"] = hooksJSON

	// Write to file
	if err := os.MkdirAll(filepath.Dir(hooksPath), 0o750); err != nil {
		return 0, fmt.Errorf("failed to create .codex directory: %w", err)
	}

	output, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("failed to marshal hooks.json: %w", err)
	}

	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return 0, fmt.Errorf("failed to write hooks.json: %w", err)
	}

	// Hooks are enabled by default in Codex (since 0.124.0), so no feature
	// flag is written. Self-heal any stale .codex/config.toml an older
	// entire version left behind.
	if err := cleanupStaleFeatureConfig(repoRoot); err != nil {
		return count, err
	}

	return count, nil
}

// UninstallHooks removes Entire hooks from Codex hooks.json.
func (c *CodexAgent) UninstallHooks(ctx context.Context) error {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path constructed from repo root
	if err != nil {
		return nil //nolint:nilerr // No hooks.json means nothing to uninstall
	}

	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err != nil {
		return fmt.Errorf("failed to parse hooks.json: %w", err)
	}

	var rawHooks map[string]json.RawMessage
	if hooksRaw, ok := topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &rawHooks); err != nil {
			return fmt.Errorf("failed to parse hooks: %w", err)
		}
	}
	if rawHooks == nil {
		return nil
	}

	var sessionStart, userPromptSubmit, stop, postToolUse []MatcherGroup
	if err := parseHookType(rawHooks, "SessionStart", &sessionStart); err != nil {
		return err
	}
	if err := parseHookType(rawHooks, "UserPromptSubmit", &userPromptSubmit); err != nil {
		return err
	}
	if err := parseHookType(rawHooks, "Stop", &stop); err != nil {
		return err
	}
	if err := parseHookType(rawHooks, "PostToolUse", &postToolUse); err != nil {
		return err
	}

	sessionStart = removeEntireHooks(sessionStart)
	userPromptSubmit = removeEntireHooks(userPromptSubmit)
	stop = removeEntireHooks(stop)
	postToolUse = removeEntireHooks(postToolUse)

	marshalHookType(rawHooks, "SessionStart", sessionStart)
	marshalHookType(rawHooks, "UserPromptSubmit", userPromptSubmit)
	marshalHookType(rawHooks, "Stop", stop)
	marshalHookType(rawHooks, "PostToolUse", postToolUse)

	if len(rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		topLevel["hooks"] = hooksJSON
	} else {
		delete(topLevel, "hooks")
	}

	output, err := jsonutil.MarshalIndentWithNewline(topLevel, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks.json: %w", err)
	}
	if err := os.WriteFile(hooksPath, output, 0o600); err != nil {
		return fmt.Errorf("failed to write hooks.json: %w", err)
	}
	return nil
}

// AreHooksInstalled checks if Entire hooks are installed in Codex hooks.json.
func (c *CodexAgent) AreHooksInstalled(ctx context.Context) bool {
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		repoRoot = "."
	}

	hooksPath := filepath.Join(repoRoot, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath) //nolint:gosec // path constructed from repo root
	if err != nil {
		return false
	}

	var hooksFile HooksFile
	if err := json.Unmarshal(data, &hooksFile); err != nil {
		return false
	}

	return hasEntireHook(hooksFile.Hooks.SessionStart) &&
		hasEntireHook(hooksFile.Hooks.UserPromptSubmit) &&
		hasEntireHook(hooksFile.Hooks.Stop) &&
		hasEntireHook(hooksFile.Hooks.PostToolUse)
}

// --- Helpers ---

func parseHookType(rawHooks map[string]json.RawMessage, hookType string, target *[]MatcherGroup) error {
	if data, ok := rawHooks[hookType]; ok {
		if err := json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("failed to parse %s hooks: %w", hookType, err)
		}
	}
	return nil
}

func marshalHookType(rawHooks map[string]json.RawMessage, hookType string, groups []MatcherGroup) {
	if len(groups) == 0 {
		delete(rawHooks, hookType)
		return
	}
	data, err := jsonutil.MarshalWithNoHTMLEscape(groups)
	if err != nil {
		return
	}
	rawHooks[hookType] = data
}

func hookCommandExists(groups []MatcherGroup, command string) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if hook.Command == command {
				return true
			}
		}
	}
	return false
}

func syncHookCommand(groups []MatcherGroup, command string) ([]MatcherGroup, bool) {
	if hookCommandExists(groups, command) {
		return groups, false
	}
	if hasEntireHook(groups) {
		groups = removeEntireHooks(groups)
	}
	return addHook(groups, command), true
}

func addHook(groups []MatcherGroup, command string) []MatcherGroup {
	entry := HookEntry{
		Type:    "command",
		Command: command,
		Timeout: 30,
	}

	// Add to an existing group with null matcher, or create a new one
	for i, group := range groups {
		if group.Matcher == nil {
			groups[i].Hooks = append(groups[i].Hooks, entry)
			return groups
		}
	}
	return append(groups, MatcherGroup{
		Matcher: nil,
		Hooks:   []HookEntry{entry},
	})
}

func isEntireHook(command string) bool {
	return agent.IsManagedHookCommand(command, entireHookPrefixes)
}

func hasEntireHook(groups []MatcherGroup) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if isEntireHook(hook.Command) {
				return true
			}
		}
	}
	return false
}

func removeEntireHooks(groups []MatcherGroup) []MatcherGroup {
	result := make([]MatcherGroup, 0, len(groups))
	for _, group := range groups {
		filtered := make([]HookEntry, 0, len(group.Hooks))
		for _, hook := range group.Hooks {
			if !isEntireHook(hook.Command) {
				filtered = append(filtered, hook)
			}
		}
		if len(filtered) > 0 {
			group.Hooks = filtered
			result = append(result, group)
		}
	}
	return result
}

// configFileName is the Codex config file name.
const configFileName = "config.toml"

// featureLine / legacyFeatureLine are the TOML feature-flag lines older
// entire versions wrote to a project-local .codex/config.toml back when
// Codex hooks were experimental (the flag was renamed from `codex_hooks`
// to `hooks` in Codex 0.129.0). Hooks are enabled by default since Codex
// 0.124.0 (openai/codex#19012), so the flag is no longer written — these
// constants only identify stale files for cleanupStaleFeatureConfig.
const (
	featureLine       = "hooks = true"
	legacyFeatureLine = "codex_hooks = true"
)

// cleanupStaleFeatureConfig removes a project-local .codex/config.toml left
// behind by an older entire version that wrote the hooks feature flag there.
// The flag is obsolete (hooks are on by default since Codex 0.124.0), and a
// leftover config.toml is actively harmful when the repo lives inside
// <CODEX_HOME>/agents — Codex recursively scans that tree for agent-role
// TOML files and rejects the leftover at every startup as a "malformed
// agent role definition" (entireio/cli#842). Only removes the file when
// every non-blank line is one of the exact feature-flag lines or the
// [features] header this package used to write (see
// isEntireManagedLocalConfig) — a file carrying any unrelated user content
// is left alone.
func cleanupStaleFeatureConfig(repoRoot string) error {
	configPath := filepath.Join(repoRoot, ".codex", configFileName)

	data, err := os.ReadFile(configPath) //nolint:gosec // path constructed from repo root
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read stale config.toml: %w", err)
	}

	if !isEntireManagedLocalConfig(string(data)) {
		return nil
	}
	if err := os.Remove(configPath); err != nil {
		return fmt.Errorf("failed to remove stale config.toml: %w", err)
	}
	return nil
}

// isEntireManagedLocalConfig reports whether content consists of nothing but
// the [features] header and feature-flag lines this package used to write
// (plus blank lines), with at least one such managed line present. Any other
// non-blank line — a user's own setting or a comment — makes the file
// unmanaged, so cleanup leaves it untouched.
//
// The check is line-anchored on purpose: a substring scan could mistake a
// user's `webhooks = true` for our `hooks = true`, or match `[features]`
// inside an unrelated value. A whole-line match cannot mistake a user's
// line for ours.
func isEntireManagedLocalConfig(content string) bool {
	managed := map[string]bool{
		"[features]":      true,
		featureLine:       true,
		legacyFeatureLine: true,
	}
	sawManaged := false
	for raw := range strings.SplitSeq(content, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if !managed[trimmed] {
			return false
		}
		sawManaged = true
	}
	return sawManaged
}
