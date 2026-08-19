package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HookTrustGaps returns the snake_case event labels declared in Codex's
// authoritative hooks.json that don't have a matching
// `[hooks.state."<hooks.json>:<event>:0:0"]` entry in the user's Codex
// config.toml — i.e. events the local user hasn't approved yet.
//
// This is the structural form of the trust check: we don't recompute
// Codex's hook hash, we only look at key presence. That misses the
// "command changed but key is still there" case (status = Modified),
// but Codex's own startup warning catches those — our purpose here is
// to surface fresh additions like "you trusted three hooks last month
// but a new PostToolUse arrived" inside our SessionStart welcome.
//
// Returns nil when:
//   - .codex/hooks.json doesn't exist (entire isn't installed in this repo)
//   - The user's config.toml can't be read
//   - Every declared event already has a state entry
func HookTrustGaps(ctx context.Context) []string {
	return InspectHookTrust(ctx).Gaps
}

// HookTrustInspection is a structural view of Codex's local approval records.
// It never computes or copies trusted hashes.
type HookTrustInspection struct {
	Declared []string
	Gaps     []string
	Known    bool
}

// InspectHookTrust reports declared events and whether the user's Codex config
// contains approval records for them. Known is false when config.toml cannot be
// read, so callers do not mistake an unavailable trust check for active hooks.
func InspectHookTrust(ctx context.Context) HookTrustInspection {
	location, err := ResolveHookLocation(ctx)
	if err != nil || !location.ProjectLayerExists() {
		return HookTrustInspection{}
	}
	return inspectHookTrust(location.HooksPath)
}

func inspectHookTrust(hooksJSONPath string) HookTrustInspection {
	declared, ok := declaredCodexEvents(hooksJSONPath)
	if !ok || len(declared) == 0 {
		return HookTrustInspection{}
	}
	inspection := HookTrustInspection{Declared: declared}

	configPath := codexConfigPath()
	if configPath == "" {
		return inspection
	}
	trusted, ok := readCodexTrustedKeys(configPath)
	if !ok {
		return inspection
	}
	inspection.Known = true

	for _, ev := range declared {
		// Match any handler index — Codex's state key is
		// "<path>:<event>:<group>:<handler>". Trust on any handler counts.
		prefix := hooksJSONPath + ":" + ev + ":"
		if !codexAnyKeyHasPrefix(trusted, prefix) {
			inspection.Gaps = append(inspection.Gaps, ev)
		}
	}
	return inspection
}

func codexConfigPath() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// declaredCodexEvents reads hooks.json and returns the snake_case labels
// of every event that has at least one handler declared. The bool reports
// whether the read+parse succeeded — false on missing/malformed file so
// callers can stay silent rather than mid-flow noise.
func declaredCodexEvents(hooksJSONPath string) ([]string, bool) {
	data, err := os.ReadFile(hooksJSONPath) //nolint:gosec // path constructed from caller-controlled repo root
	if err != nil {
		return nil, false
	}
	var file HooksFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, false
	}
	var events []string
	add := func(label string, groups []MatcherGroup) {
		for _, g := range groups {
			if len(g.Hooks) > 0 {
				events = append(events, label)
				return
			}
		}
	}
	add("session_start", file.Hooks.SessionStart)
	add("session_end", file.Hooks.SessionEnd)
	add("user_prompt_submit", file.Hooks.UserPromptSubmit)
	add("stop", file.Hooks.Stop)
	add("pre_tool_use", file.Hooks.PreToolUse)
	add("post_tool_use", file.Hooks.PostToolUse)
	return events, true
}

// MissingEntireHooks returns the snake_case event labels the CLI's canonical
// install ships today that aren't backed by an Entire-managed hook command in
// Codex's authoritative hooks.json. Surfaces drift when the user enabled Codex
// on an older release and the install set has since grown.
//
// Returns nil when hooks.json is missing or unreadable — those cases
// are "Codex isn't enabled here", which is a different problem.
func MissingEntireHooks(ctx context.Context) []string {
	location, err := ResolveHookLocation(ctx)
	if err != nil {
		return nil
	}
	return missingEntireHooks(location.HooksPath)
}

func missingEntireHooks(hooksJSONPath string) []string {
	data, err := os.ReadFile(hooksJSONPath) //nolint:gosec // path constructed from caller-controlled repo root
	if err != nil {
		return nil
	}
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err != nil {
		return nil
	}
	var rawHooks map[string]json.RawMessage
	if err := json.Unmarshal(topLevel["hooks"], &rawHooks); err != nil {
		return nil
	}
	var missing []string
	for _, hook := range managedHooks {
		var groups []MatcherGroup
		if err := parseHookType(rawHooks, hook.event, &groups); err != nil {
			return nil
		}
		if !hasEntireHook(groups) {
			missing = append(missing, hook.label)
		}
	}
	return missing
}

// HasLegacyEntireHooks reports whether this linked checkout still has an
// Entire-managed hook in the obsolete worktree-local file.
func HasLegacyEntireHooks(ctx context.Context) bool {
	location, err := ResolveHookLocation(ctx)
	if err != nil || location.LegacyHooksPath == "" {
		return false
	}
	return hasEntireHooksAtPath(location.LegacyHooksPath)
}

// HasWorktreeLocalEntireHooks reports whether the current checkout has an
// Entire-managed Codex hook without assuming that location is authoritative.
func HasWorktreeLocalEntireHooks(ctx context.Context) bool {
	path, err := worktreeLocalHooksPath(ctx)
	return err == nil && hasEntireHooksAtPath(path)
}

func hasEntireHooksAtPath(path string) bool {
	document, err := readHooksDocument(path)
	if err != nil || !document.exists {
		return false
	}
	for _, raw := range document.rawHooks {
		var groups []MatcherGroup
		if json.Unmarshal(raw, &groups) == nil && hasEntireHook(groups) {
			return true
		}
	}
	return false
}

// codexTrustStateHeaderRegex matches `[hooks.state."<key>"]` headers in
// the user's Codex config.toml. Quote-only — Codex's own writer emits
// quoted keys (codex-rs/tui/src/app/background_requests.rs:874), and
// looser parsing would invite false matches in user-edited configs.
var codexTrustStateHeaderRegex = regexp.MustCompile(`(?m)^\[hooks\.state\."([^"]+)"\]`)

func readCodexTrustedKeys(configPath string) (map[string]struct{}, bool) {
	data, err := os.ReadFile(configPath) //nolint:gosec // path resolved from CODEX_HOME or HOME
	if err != nil {
		return nil, false
	}
	keys := make(map[string]struct{})
	for _, m := range codexTrustStateHeaderRegex.FindAllStringSubmatch(string(data), -1) {
		keys[m[1]] = struct{}{}
	}
	return keys, true
}

func codexAnyKeyHasPrefix(keys map[string]struct{}, prefix string) bool {
	for k := range keys {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}
