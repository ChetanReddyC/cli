package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
	"github.com/gofrs/flock"
)

// HooksFileName is the hooks config file used by Codex.
const HooksFileName = "hooks.json"

const maxHooksFileBytes = 1 << 20

// defaultHookTimeoutSec is the timeout Entire configures for Codex hooks that
// run between turns, where Codex allows up to its standard 600s.
const defaultHookTimeoutSec = 30

const hooksLockWaitTimeout = 5 * time.Second

// managedHook describes one hooks.json event Entire owns. Keeping the event
// key, verb, timeout and production wrapper together means adding or removing
// an event is a single table edit rather than parallel edits in InstallHooks,
// UninstallHooks and AreHooksInstalled.
type managedHook struct {
	event   string // hooks.json key
	label   string // Codex trust-state key
	verb    string // `entire hooks codex <verb>`
	timeout int
	wrap    func(cmd string, windows bool) string

	// core marks the events whose absence means Codex was never enabled in this
	// repo, as opposed to enabled against an older release that installed fewer
	// events. Only these gate AreHooksInstalled — see the comment there.
	core bool
}

// managedHooks is the full set of Codex events Entire installs.
var managedHooks = []managedHook{
	{event: "SessionStart", label: "session_start", verb: HookNameSessionStart, timeout: defaultHookTimeoutSec, core: true, wrap: func(cmd string, windows bool) string {
		return agent.WrapProductionJSONWarningHookCommandForOS(cmd, agent.WarningFormatSingleLine, windows)
	}},
	// SessionEnd is the one event Codex clamps: it caps handlers at
	// SESSION_END_MAX_TIMEOUT_SEC and warns at every startup when a config asks
	// for more, so it is installed at exactly the ceiling. See SessionEndTimeoutSec.
	//
	// Not core: it postdates the four events below, so requiring it would
	// un-enable Codex for everyone who enabled it before this release.
	{event: "SessionEnd", label: "session_end", verb: HookNameSessionEnd, timeout: SessionEndTimeoutSec, wrap: agent.WrapProductionSilentHookCommandForOS},
	{event: "UserPromptSubmit", label: "user_prompt_submit", verb: HookNameUserPromptSubmit, timeout: defaultHookTimeoutSec, core: true, wrap: agent.WrapProductionSilentHookCommandForOS},
	{event: "Stop", label: "stop", verb: HookNameStop, timeout: defaultHookTimeoutSec, core: true, wrap: agent.WrapProductionSilentHookCommandForOS},
	{event: "PostToolUse", label: "post_tool_use", verb: HookNamePostToolUse, timeout: defaultHookTimeoutSec, core: true, wrap: agent.WrapProductionSilentHookCommandForOS},
	// Codex keys hooks.json by PascalCase event name (its own fixtures do the
	// same), even though HookEventName serializes snake_case elsewhere in its
	// protocol — following that would install hooks that never fire.
	//
	// Not core, for the same reason as SessionEnd: both postdate the four events
	// above, so requiring them would un-enable Codex for anyone who enabled it
	// before this release.
	{event: "SubagentStart", label: "subagent_start", verb: HookNameSubagentStart, timeout: defaultHookTimeoutSec, wrap: agent.WrapProductionSilentHookCommandForOS},
	{event: "SubagentStop", label: "subagent_stop", verb: HookNameSubagentStop, timeout: defaultHookTimeoutSec, wrap: agent.WrapProductionSilentHookCommandForOS},
}

// InstallHooks installs Entire hooks in the current checkout's .codex/hooks.json.
func (c *CodexAgent) InstallHooks(ctx context.Context, force bool) (int, error) {
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	if err != nil {
		return 0, err
	}
	if err := ensureWorktreeProjectDir(worktreeHooks); err != nil {
		return 0, fmt.Errorf("create worktree .codex directory: %w", err)
	}
	release, err := acquireHooksLock(ctx, worktreeHooks.Path()+".lock")
	if err != nil {
		return 0, fmt.Errorf("lock Codex hooks file: %w", err)
	}
	defer release()

	destination, err := readWorktreeHooksDocument(worktreeHooks)
	if err != nil {
		return 0, err
	}

	count, err := installManagedHooks(ctx, destination, force)
	if err != nil {
		return 0, err
	}
	if count > 0 {
		if err := writeHooksDocument(worktreeHooks, destination); err != nil {
			return 0, err
		}
	}

	// No .codex/config.toml is written: hooks are enabled by default in
	// Codex (since 0.124.0), and a TOML file inside Codex's reserved
	// <CODEX_HOME>/agents tree would be rejected by its agent-role scanner
	// at every startup (entireio/cli#842). A leftover config.toml written
	// by an older entire version must be removed manually.
	return count, nil
}

// UninstallHooks removes Entire hooks from the current checkout only.
func (c *CodexAgent) UninstallHooks(ctx context.Context) error {
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	if err != nil {
		return err
	}
	return uninstallWorktreeHooksFile(ctx, worktreeHooks)
}

func uninstallWorktreeHooksFile(ctx context.Context, worktreeHooks WorktreeHooksPath) error {
	hasFile, err := worktreeHooksMayExist(worktreeHooks)
	if err != nil {
		return err
	}
	if !hasFile {
		return nil
	}
	release, err := acquireHooksLock(ctx, worktreeHooks.Path()+".lock")
	if err != nil {
		return fmt.Errorf("lock Codex hooks file: %w", err)
	}
	defer release()

	document, err := readWorktreeHooksDocument(worktreeHooks)
	if err != nil {
		return err
	}
	if !document.exists {
		return nil
	}
	changed, err := removeEntireHooksFromDocument(document)
	if err != nil {
		return err
	}
	if changed {
		return writeHooksDocument(worktreeHooks, document)
	}
	return nil
}

// AreHooksInstalled reports whether Codex is wired up to Entire in this repo.
//
// It requires only the core events, not everything InstallHooks writes today.
// The two questions are different: this one decides whether Codex is listed as
// an installed agent (`entire status`, the review and investigate pickers), and
// answering it with the full set would drop Codex out of all of them the moment
// a release adds an event — every existing install predates the addition. Drift
// against today's set is part of hook-config inspection, which `entire doctor`
// reports with the fix (`entire enable`).
func (c *CodexAgent) AreHooksInstalled(ctx context.Context) bool {
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	if err != nil {
		return false
	}
	inspection := inspectWorktreeHookConfig(ctx, worktreeHooks)
	return inspection.State == HookFileEntire && inspection.CoreInstalled
}

// CheckHookConfig reports whether the current checkout's Codex hook
// configuration is absent, current, or needs installation.
func (c *CodexAgent) CheckHookConfig(ctx context.Context) agent.HookConfigState {
	worktreeHooks, err := ResolveWorktreeHooksPath(ctx)
	if err != nil {
		return agent.HooksAbsent
	}
	inspection := inspectWorktreeHookConfig(ctx, worktreeHooks)
	switch inspection.State {
	case HookFileInvalid:
		return agent.HooksAbsent
	case HookFileEntire:
		if inspection.Current {
			return agent.HooksCurrent
		}
		return agent.HooksOutdated
	case HookFileAbsent, HookFileUserOnly:
		return agent.HooksAbsent
	}
	return agent.HooksAbsent
}

type managedHookSpec struct {
	event   string
	label   string
	command string
	timeout int
	core    bool
}

// HookFileState distinguishes Entire's installation from unrelated user
// configuration and from a file that cannot be inspected safely.
type HookFileState uint8

const (
	HookFileAbsent HookFileState = iota
	HookFileUserOnly
	HookFileEntire
	HookFileInvalid
)

// HookConfigInspection is the single parsed view used by Codex presence,
// freshness, missing-hook, and doctor reporting.
type HookConfigInspection struct {
	State         HookFileState
	Missing       []string
	Declared      []string
	Current       bool
	CoreInstalled bool
	Err           error
}

type hooksDocument struct {
	topLevel map[string]json.RawMessage
	rawHooks map[string]json.RawMessage
	exists   bool
}

func managedHookSpecs(ctx context.Context) []managedHookSpec {
	const cmdPrefix = "entire hooks codex "
	useWindowsProductionHooks := agent.UseWindowsProductionHooks(ctx)
	specs := make([]managedHookSpec, 0, len(managedHooks))
	for _, hook := range managedHooks {
		command := hook.wrap(cmdPrefix+hook.verb, useWindowsProductionHooks)
		specs = append(specs, managedHookSpec{
			event:   hook.event,
			label:   hook.label,
			command: command,
			timeout: hook.timeout,
			core:    hook.core,
		})
	}
	return specs
}

// InspectHookConfig resolves and parses the hooks file Codex discovers.
func InspectHookConfig(ctx context.Context) HookConfigInspection {
	discovery := ResolveHookDiscovery(ctx)
	if discovery.State != HookDiscoveryResolved {
		return HookConfigInspection{State: HookFileInvalid, Err: discovery.Diagnostic}
	}
	return inspectDiscoveredHookConfig(ctx, discovery.DiscoveredHooks)
}

func inspectWorktreeHookConfig(ctx context.Context, hooks WorktreeHooksPath) HookConfigInspection {
	projectDir, err := validateWorktreeHookTarget(hooks)
	if err != nil {
		return HookConfigInspection{State: HookFileInvalid, Err: err}
	}
	if err := validateExistingProjectDir(projectDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HookConfigInspection{State: HookFileAbsent}
		}
		return HookConfigInspection{State: HookFileInvalid, Err: err}
	}
	return inspectHookConfigAt(ctx, hooks.Path())
}

func inspectDiscoveredHookConfig(ctx context.Context, hooks DiscoveredHooksPath) HookConfigInspection {
	if err := validateDiscoveredHookTarget(hooks); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HookConfigInspection{State: HookFileAbsent}
		}
		return HookConfigInspection{State: HookFileInvalid, Err: err}
	}
	return inspectHookConfigAt(ctx, hooks.Path())
}

func inspectHookConfigAt(ctx context.Context, path string) HookConfigInspection {
	document, err := readHooksDocument(path)
	if err != nil {
		return HookConfigInspection{State: HookFileInvalid, Err: err}
	}
	if !document.exists {
		return HookConfigInspection{State: HookFileAbsent}
	}

	inspection := HookConfigInspection{
		State:         HookFileUserOnly,
		Current:       true,
		CoreInstalled: true,
	}
	inspection.Declared, err = declaredCodexEventsFromDocument(document)
	if err != nil {
		return HookConfigInspection{State: HookFileInvalid, Err: err}
	}
	for _, spec := range managedHookSpecs(ctx) {
		var groups []MatcherGroup
		if err := parseHookType(document.rawHooks, spec.event, &groups); err != nil {
			return HookConfigInspection{State: HookFileInvalid, Err: err}
		}
		installed := hasEntireHook(groups)
		if installed {
			inspection.State = HookFileEntire
		} else {
			inspection.Missing = append(inspection.Missing, spec.label)
			if spec.core {
				inspection.CoreInstalled = false
			}
		}
		if !managedHookIsCurrent(groups, spec.command, spec.timeout) {
			inspection.Current = false
		}
	}
	if inspection.State == HookFileUserOnly {
		inspection.Missing = nil
		inspection.Current = false
		inspection.CoreInstalled = false
	}
	return inspection
}

func readHooksDocument(path string) (*hooksDocument, error) {
	document := &hooksDocument{
		topLevel: make(map[string]json.RawMessage),
		rawHooks: make(map[string]json.RawMessage),
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Codex project directory for %q: %w", path, err)
	}
	defer root.Close()

	name := filepath.Base(path)
	before, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect Codex hooks file %q: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("codex hooks path %q is not a regular file", path)
	}
	if before.Size() > maxHooksFileBytes {
		return nil, fmt.Errorf("codex hooks file %q exceeds %d bytes", path, maxHooksFileBytes)
	}

	file, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("open Codex hooks file %q: %w", path, err)
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened Codex hooks file %q: %w", path, err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("codex hooks file %q changed while opening", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHooksFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Codex hooks file %q: %w", path, err)
	}
	if len(data) > maxHooksFileBytes {
		return nil, fmt.Errorf("codex hooks file %q exceeds %d bytes", path, maxHooksFileBytes)
	}
	after, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("reinspect Codex hooks file %q: %w", path, err)
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fmt.Errorf("codex hooks file %q changed while reading", path)
	}
	document.exists = true
	if err := json.Unmarshal(data, &document.topLevel); err != nil {
		return nil, fmt.Errorf("failed to parse existing hooks.json %q: %w", path, err)
	}
	if document.topLevel == nil {
		document.topLevel = make(map[string]json.RawMessage)
	}
	if hooksRaw, ok := document.topLevel["hooks"]; ok {
		if err := json.Unmarshal(hooksRaw, &document.rawHooks); err != nil {
			return nil, fmt.Errorf("failed to parse hooks in hooks.json %q: %w", path, err)
		}
	}
	if document.rawHooks == nil {
		document.rawHooks = make(map[string]json.RawMessage)
	}
	return document, nil
}

func installManagedHooks(ctx context.Context, document *hooksDocument, force bool) (int, error) {
	count := 0
	for _, spec := range managedHookSpecs(ctx) {
		var groups []MatcherGroup
		if err := parseHookType(document.rawHooks, spec.event, &groups); err != nil {
			return 0, err
		}
		if force {
			groups = removeEntireHooks(groups)
		}
		updated, changed := syncHookCommand(groups, spec.command, spec.timeout)
		if changed {
			marshalHookType(document.rawHooks, spec.event, updated)
			count++
		}
	}
	return count, nil
}

func removeEntireHooksFromDocument(document *hooksDocument) (bool, error) {
	managedEvents := make(map[string]struct{})
	for _, hook := range managedHooks {
		managedEvents[hook.event] = struct{}{}
	}
	changed := false
	for event, raw := range document.rawHooks {
		var groups []MatcherGroup
		if err := json.Unmarshal(raw, &groups); err != nil {
			if _, managed := managedEvents[event]; managed {
				return false, fmt.Errorf("failed to parse %s hooks: %w", event, err)
			}
			continue
		}
		if !hasEntireHook(groups) {
			continue
		}
		updated := removeEntireHooks(groups)
		marshalHookType(document.rawHooks, event, updated)
		changed = true
	}
	return changed, nil
}

func readWorktreeHooksDocument(worktreeHooks WorktreeHooksPath) (*hooksDocument, error) {
	destination, err := resolveHookDestination(worktreeHooks)
	if err != nil {
		return nil, err
	}
	return readHooksDocument(destination.path)
}

func writeHooksDocument(worktreeHooks WorktreeHooksPath, document *hooksDocument) error {
	if len(document.rawHooks) > 0 {
		hooksJSON, err := jsonutil.MarshalWithNoHTMLEscape(document.rawHooks)
		if err != nil {
			return fmt.Errorf("failed to marshal hooks: %w", err)
		}
		document.topLevel["hooks"] = hooksJSON
	} else {
		delete(document.topLevel, "hooks")
	}
	if len(document.topLevel) == 0 {
		destination, err := resolveHookDestination(worktreeHooks)
		if err != nil {
			return err
		}
		if !destination.exists {
			return nil
		}
		if err := os.Remove(destination.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty hooks.json: %w", err)
		}
		return nil
	}
	if err := ensureWorktreeProjectDir(worktreeHooks); err != nil {
		return fmt.Errorf("create .codex directory: %w", err)
	}
	output, err := jsonutil.MarshalIndentWithNewline(document.topLevel, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal hooks.json: %w", err)
	}
	destination, err := resolveHookDestination(worktreeHooks)
	if err != nil {
		return err
	}
	if err := jsonutil.WriteFileAtomic(destination.path, output, destination.mode); err != nil {
		return fmt.Errorf("failed to write hooks.json: %w", err)
	}
	document.exists = true
	return nil
}

func acquireHooksLock(ctx context.Context, path string) (func(), error) {
	return acquireHooksLockWithin(ctx, path, hooksLockWaitTimeout)
}

func acquireHooksLockWithin(ctx context.Context, path string, wait time.Duration) (func(), error) {
	const retryDelay = 25 * time.Millisecond
	lockCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	lock := flock.New(path)
	locked, err := lock.TryLockContext(lockCtx, retryDelay)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, fmt.Errorf(
				"timed out after %s waiting for another Entire process to update Codex hooks: %w",
				wait,
				err,
			)
		}
		return nil, fmt.Errorf("acquire file lock: %w", err)
	}
	if !locked {
		return nil, errors.New("acquire file lock: lock unavailable")
	}
	return func() { _ = lock.Unlock() }, nil //nolint:errcheck // The completed mutation cannot be rolled back if unlock reports an error.
}

func managedHookIsCurrent(groups []MatcherGroup, command string, timeoutSec int) bool {
	count := 0
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if !isEntireHook(hook.Command) {
				continue
			}
			if hook.Command != command || hook.Timeout != timeoutSec {
				return false
			}
			count++
		}
	}
	return count == 1
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

// hookCommandExists reports whether the exact command is already configured
// with the timeout we want. The timeout is part of the match so an upgrade
// rewrites a hook installed by an older Entire with a different budget —
// notably SessionEnd, where a leftover 30s makes Codex print a clamping warning
// at every startup.
func hookCommandExists(groups []MatcherGroup, command string, timeoutSec int) bool {
	for _, group := range groups {
		for _, hook := range group.Hooks {
			if hook.Command == command && hook.Timeout == timeoutSec {
				return true
			}
		}
	}
	return false
}

// syncHookCommand ensures groups contains exactly the given Entire hook command
// at the given timeout, and no other Entire-owned entry, reporting whether the
// config changed.
//
// Stale entries are dropped even when command is already present. Checking
// presence first (as this did before) left a hook written by an older version
// sitting next to the current one, so both fired — for the removed local-dev mode
// that meant a script inside the working tree kept running on every agent turn.
func syncHookCommand(groups []MatcherGroup, command string, timeoutSec int) ([]MatcherGroup, bool) {
	groups, dropped := dropStaleEntireHooks(groups, command, timeoutSec)
	if hookCommandExists(groups, command, timeoutSec) {
		return groups, dropped
	}
	return addHook(groups, command, timeoutSec), true
}

// dropStaleEntireHooks removes Entire-owned hooks that are not command at
// timeoutSec, per matcher group, pruning groups left with no hooks. See
// agent.DropStaleManagedHooks for why this runs on every install.
//
// The timeout is part of what counts as stale here, which the shared helper
// cannot express: it matches on the command alone, and Codex budgets per event.
// A SessionEnd hook left at the old 30s keeps its command but makes Codex print
// a clamping warning at every startup — see SessionEndTimeoutSec.
func dropStaleEntireHooks(groups []MatcherGroup, command string, timeoutSec int) ([]MatcherGroup, bool) {
	staleTimeout := func(e HookEntry) bool { return e.Command == command && e.Timeout != timeoutSec }

	result := make([]MatcherGroup, 0, len(groups))
	dropped := false
	for _, group := range groups {
		kept, d := agent.DropStaleManagedHooks(group.Hooks, hookEntryCommand, []string{command})
		if d {
			dropped = true
		}
		// Clone before deleting: with nothing dropped above, kept still aliases
		// the caller's slice.
		if slices.ContainsFunc(kept, staleTimeout) {
			kept = slices.DeleteFunc(slices.Clone(kept), staleTimeout)
			dropped = true
		}
		if len(kept) > 0 {
			group.Hooks = kept
			result = append(result, group)
		}
	}
	if !dropped {
		return groups, false
	}
	return result, true
}

// hookEntryCommand reads the command off a hook entry for the shared helpers.
func hookEntryCommand(e HookEntry) string { return e.Command }

func addHook(groups []MatcherGroup, command string, timeoutSec int) []MatcherGroup {
	entry := HookEntry{
		Type:    "command",
		Command: command,
		Timeout: timeoutSec,
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
	return agent.IsManagedHookCommand(command)
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
