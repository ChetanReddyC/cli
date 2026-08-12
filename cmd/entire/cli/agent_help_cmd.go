package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
)

// agentHelpAnnotation marks an otherwise-hidden command as worth advertising to
// coding agents through `entire agent-help`. Hidden commands (e.g. trail) opt in
// by setting Annotations[agentHelpAnnotation] = "true".
const agentHelpAnnotation = "entire_agent_help"

// agentHelpRequiresTrailsAnnotation marks a command whose surface should only be
// advertised to agents when trails are enabled for the repo. While the trails
// product may not be available to a user yet, agent-help must not point agents at
// commands they can't use — so trail-gated commands are hidden until the same
// "is trails enabled" signal the first-turn injection already gates on says yes.
const agentHelpRequiresTrailsAnnotation = "entire_agent_help_requires_trails"

// agentHelpAnnotationEnabled is the truthy value for the agent-help annotations.
const agentHelpAnnotationEnabled = "true"

// agentHelpOverview is the only hand-maintained prose in agent-help: a terse,
// high-level "what entire is for" plus the standing repo-inference rule. It names
// no flags or subcommands — those are rendered live from the installed command
// tree — so it changes only when a whole capability area lands, not when a flag
// is added.
const agentHelpOverview = `Entire's CLI is the source of truth for its own usage. Do not guess flags or
subcommands — read them from this command. You are already inside the repo:
entire auto-detects it from the git origin remote, so never ask the user for the
repo name. Pass --repo only to target a DIFFERENT repo.`

// agentHelpAudience answers the question an agent actually has when it reads this
// listing: may I run this without being asked? A flat alphabetical dump of every
// command cannot answer it, so an agent either runs nothing or runs something it
// should have left alone (`entire enable`, `entire review`, `entire org create`).
// Grouping by initiator puts that judgment in the CLI, where it is maintained
// once, rather than in each agent's guesswork or in the first-turn injection,
// which pays for it on every session.
type agentHelpAudience int

const (
	// agentHelpAudienceReadOnly: inspection only. Safe to run unprompted whenever
	// it would inform the work; cannot change repo, account, or Entire state.
	agentHelpAudienceReadOnly agentHelpAudience = iota
	// agentHelpAudienceTaskDriven: part of doing the work, but it writes data or
	// spends tokens. Run when the task calls for it, not speculatively.
	agentHelpAudienceTaskDriven
	// agentHelpAudienceUserOwned: setup, auth, account/admin, or destructive. The
	// agent may suggest these but must not run them on its own initiative.
	agentHelpAudienceUserOwned
)

// agentHelpAudiences classifies commands by space-separated path relative to the
// root ("status", "checkpoint list"). It is a single table rather than a
// per-command Annotations field so the whole policy is reviewable in one place —
// the classification is a judgment call and reads as one only when the commands
// sit side by side.
//
// A GROUP is read-only only if every advertised subcommand is, so `checkpoint`
// and `session` are task-driven even though their Short help reads as pure
// inspection: `checkpoint policy` updates policy, and `session` carries
// adopt/attach/resume/stop. Demoting the whole group would have hidden the
// inspection commands an agent most wants (`checkpoint list`, `session list`),
// so their read-only subcommands are classified individually and the listing
// breaks them out — see agentHelpEntries. The mutating siblings are classified
// too; they stay collapsed under the group in the listing but carry an accurate
// audience in `agent-help <group> --json`.
//
// Unlisted commands fall back to agentHelpAudienceUserOwned (see
// agentHelpAudienceFor): the fail-safe direction is an agent declining to run
// something it could have, never running something it should not have.
// TestAgentHelpAudiences_CoverEveryAdvertisedCommand fails CI when a new
// top-level command lands unclassified, so the fallback is a backstop and not
// the normal path.
var agentHelpAudiences = map[string]agentHelpAudience{
	// Read-only inspection, every subcommand.
	"activity": agentHelpAudienceReadOnly,
	"blame":    agentHelpAudienceReadOnly,
	"experts":  agentHelpAudienceReadOnly,
	"labs":     agentHelpAudienceReadOnly,
	"recap":    agentHelpAudienceReadOnly,
	"search":   agentHelpAudienceReadOnly,
	"status":   agentHelpAudienceReadOnly,
	"tokens":   agentHelpAudienceReadOnly,
	"version":  agentHelpAudienceReadOnly,
	"why":      agentHelpAudienceReadOnly,

	// Task-driven groups whose read-only subcommands are broken out below.
	"checkpoint":         agentHelpAudienceTaskDriven,
	"checkpoint explain": agentHelpAudienceReadOnly,
	"checkpoint list":    agentHelpAudienceReadOnly,
	"checkpoint search":  agentHelpAudienceReadOnly,
	"checkpoint tokens":  agentHelpAudienceReadOnly,
	"checkpoint policy":  agentHelpAudienceTaskDriven, // "Inspect and update"
	"session":            agentHelpAudienceTaskDriven,
	"session current":    agentHelpAudienceReadOnly,
	"session info":       agentHelpAudienceReadOnly,
	"session list":       agentHelpAudienceReadOnly,
	"session tokens":     agentHelpAudienceReadOnly,
	"session adopt":      agentHelpAudienceTaskDriven,
	"session attach":     agentHelpAudienceTaskDriven,
	"session resume":     agentHelpAudienceTaskDriven, // switches branch
	"session stop":       agentHelpAudienceTaskDriven,

	// Task-driven: at least one subcommand writes data or spends tokens.
	"api":      agentHelpAudienceTaskDriven,
	"dispatch": agentHelpAudienceTaskDriven,
	"doctor":   agentHelpAudienceTaskDriven,
	"import":   agentHelpAudienceTaskDriven,
	"runner":   agentHelpAudienceTaskDriven,
	"trail":    agentHelpAudienceTaskDriven,

	// The user's: setup, auth, account/admin, destructive, or expensive enough
	// that starting one uninvited is the user's call. review and investigate spawn
	// multi-agent runs that spend real money, so they are opt-in like setup is.
	"agent":       agentHelpAudienceUserOwned,
	"auth":        agentHelpAudienceUserOwned,
	"clean":       agentHelpAudienceUserOwned,
	"configure":   agentHelpAudienceUserOwned,
	"disable":     agentHelpAudienceUserOwned,
	"enable":      agentHelpAudienceUserOwned,
	"grant":       agentHelpAudienceUserOwned,
	"investigate": agentHelpAudienceUserOwned,
	"login":       agentHelpAudienceUserOwned,
	"logout":      agentHelpAudienceUserOwned,
	"org":         agentHelpAudienceUserOwned,
	"plugin":      agentHelpAudienceUserOwned,
	"project":     agentHelpAudienceUserOwned,
	"repo":        agentHelpAudienceUserOwned,
	"review":      agentHelpAudienceUserOwned,
}

// agentHelpBreakoutGroups are the task-driven groups whose read-only
// subcommands are listed individually in the top-level listing. Membership is
// declared rather than inferred from "has classified children" so adding a
// classification to some unrelated subcommand cannot silently change what the
// listing shows, and so
// TestAgentHelpAudiences_CoverEveryAdvertisedCommand knows which groups must
// classify every child.
var agentHelpBreakoutGroups = map[string]struct{}{
	"checkpoint": {},
	"session":    {},
}

// agentHelpAudienceSections renders in this order: what an agent may do on its
// own first, what it must not do last.
var agentHelpAudienceSections = []struct {
	audience agentHelpAudience
	heading  string
	slug     string
}{
	{agentHelpAudienceReadOnly, "Safe to run on your own — read-only, changes nothing:", "read-only"},
	{agentHelpAudienceTaskDriven, "Run when the task calls for it — some subcommands write data or spend tokens:", "task-driven"},
	{agentHelpAudienceUserOwned, "The user's to run — suggest it, don't run it (setup, auth, admin, destructive):", "user-owned"},
}

// agentHelpAudienceFor classifies one command path, defaulting unlisted commands
// to user-owned so an unclassified addition is under- rather than
// over-advertised.
func agentHelpAudienceFor(path string) agentHelpAudience {
	if a, ok := agentHelpAudiences[path]; ok {
		return a
	}
	return agentHelpAudienceUserOwned
}

// agentHelpClassified reports an explicit classification only. The --json path
// uses it so an unclassified subcommand omits the field rather than asserting a
// user-owned default the table never actually made.
func agentHelpClassified(path string) (agentHelpAudience, bool) {
	a, ok := agentHelpAudiences[path]
	return a, ok
}

// agentHelpPath is a command's path relative to the root, the key shape used by
// agentHelpAudiences ("status", "checkpoint list").
func agentHelpPath(cmd *cobra.Command) string {
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

// agentHelpEntry is one line of the top-level listing: a command path, its Short
// help, and the bucket it belongs in.
type agentHelpEntry struct {
	path     string
	short    string
	audience agentHelpAudience
}

// agentHelpEntries builds the top-level listing. Every advertised top-level
// command appears; additionally, a read-only subcommand of a task-driven group is
// broken out as its own entry so the safe-unprompted inspection commands inside
// `checkpoint` and `session` are visible without drilling in. Mutating
// subcommands stay collapsed under their group — the listing exists to answer
// "what may I run on my own?", and enumerating what an agent must NOT run adds
// lines without adding an answer.
func agentHelpEntries(rootCmd *cobra.Command, trailsEnabled bool) []agentHelpEntry {
	var out []agentHelpEntry
	for _, sub := range agentHelpCommands(rootCmd, trailsEnabled) {
		path := agentHelpPath(sub)
		audience := agentHelpAudienceFor(path)
		out = append(out, agentHelpEntry{path: path, short: sub.Short, audience: audience})
		if _, ok := agentHelpBreakoutGroups[path]; !ok {
			continue
		}
		for _, child := range agentHelpCommands(sub, trailsEnabled) {
			childPath := agentHelpPath(child)
			if a, ok := agentHelpClassified(childPath); ok && a == agentHelpAudienceReadOnly {
				out = append(out, agentHelpEntry{path: childPath, short: child.Short, audience: a})
			}
		}
	}
	return out
}

// agentHelpAudienceSlug is the stable machine-readable form emitted in --json,
// so an agent parsing the structured output gets the same guidance as one
// reading the text (the repo's agent-safe-fallback rule).
func agentHelpAudienceSlug(a agentHelpAudience) string {
	for _, s := range agentHelpAudienceSections {
		if s.audience == a {
			return s.slug
		}
	}
	return "user-owned"
}

// newAgentHelpCmd builds the `entire agent-help` command. It is visible in
// `entire help` (so agents on transports without context injection can still
// find it) and renders agent-facing usage live from rootCmd's command tree.
func newAgentHelpCmd(rootCmd *cobra.Command) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "agent-help [command...]",
		Short: "Machine-readable usage for coding agents (always matches the installed CLI)",
		Long: `Prints agent-facing usage for the Entire CLI, generated live from the installed
command tree so it always matches this binary. With no arguments it prints a
high-level map of when to use entire and which subcommand; pass a command path
(e.g. "agent-help checkpoint") to see that command's exact, current flags.`,
		RunE: func(c *cobra.Command, args []string) error {
			// Resolve the origin remote once and derive both the repo line and the
			// trails-enablement check from it (avoids two git subprocesses per run).
			repoLine, trailsEnabled := agentHelpRepoContext(c.Context())
			out, err := runAgentHelp(rootCmd, args, repoLine, asJSON, trailsEnabled)
			if err != nil {
				return err
			}
			fmt.Fprint(c.OutOrStdout(), out)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit structured JSON instead of text")
	return cmd
}

// agentHelpRepoContext resolves the origin remote ONCE and derives both the repo
// line (forge/owner/repo, or "" when it can't be determined — no origin /
// detached HEAD — so the renderer degrades gracefully) and whether trails are
// enabled for that scope. Unlike the prompt-path gate, agent-help is an explicit
// command and can afford to refresh an absent or stale enablement decision rather
// than incorrectly treating an unknown cache entry as "trails unavailable".
func agentHelpRepoContext(ctx context.Context) (repoLine string, trailsEnabled bool) {
	return agentHelpRepoContextWithRefresh(ctx, refreshAgentHelpTrailsEnabledCacheIfStaleForScope)
}

// refreshAgentHelpTrailsEnabledCacheIfStaleForScope refreshes synchronously
// because agent-help is an explicit command whose output must reflect the
// current availability decision. SessionStart uses the detached
// refreshTrailsEnabledCacheIfStaleForScope path instead to avoid hook latency.
func refreshAgentHelpTrailsEnabledCacheIfStaleForScope(ctx context.Context, scope trailEnablementScope) error {
	if cachedTrailsEnablementForScope(ctx, scope, time.Now()) != trailEnablementCacheUnknown {
		return nil
	}
	if !scope.Supported {
		return saveTrailsEnabledForScope(ctx, scope, false, time.Now())
	}
	client, err := NewAuthenticatedAPIClient(ctx, false)
	if err != nil {
		return err
	}
	_, err = refreshTrailsEnabledCacheForScope(ctx, client, scope)
	return err
}

// agentHelpRepoContextWithRefresh keeps the refresh dependency explicit so the
// cache-miss behavior can be tested without authenticating against a real API.
func agentHelpRepoContextWithRefresh(
	ctx context.Context,
	refresh func(context.Context, trailEnablementScope) error,
) (repoLine string, trailsEnabled bool) {
	scope, err := currentTrailEnablementScope(ctx)
	if err != nil {
		return "", false
	}
	if scope.Forge != "" && scope.Owner != "" && scope.Repo != "" {
		repoLine = scope.RepoKey
	}

	now := time.Now()
	if decision := cachedTrailsEnablementForScope(ctx, scope, now); decision != trailEnablementCacheUnknown {
		return repoLine, decision == trailEnablementCacheEnabled
	}

	// ResolveDataAPIToken performs data-host discovery before it can reject a
	// missing login. The scope already carries the locally resolved auth identity,
	// so avoid making an unauthenticated first run wait on a network request that
	// cannot produce an enabled decision.
	if scope.AuthKey == "" {
		return repoLine, false
	}
	if recentAgentHelpTrailsRefreshFailure(ctx, scope, now) {
		return repoLine, false
	}

	refreshCtx, cancel := context.WithTimeout(ctx, trailEnablementRefreshTimeout)
	defer cancel()
	if err := refresh(refreshCtx, scope); err != nil {
		// A separate short backoff keeps an offline authenticated user from paying
		// this timeout on every agent-help invocation. It must not alter the shared
		// enablement decision, which SessionStart uses for context injection.
		if cacheErr := saveAgentHelpTrailsRefreshFailure(ctx, scope, time.Now()); cacheErr != nil {
			logging.Debug(ctx, "failed to save agent-help trails refresh backoff", "error", cacheErr)
		}
		return repoLine, false
	}
	return repoLine, cachedTrailsEnablementForScope(ctx, scope, time.Now()) == trailEnablementCacheEnabled
}

// runAgentHelp resolves args to a command node and renders it. It is pure (no
// git / IO): the caller passes the already-resolved repoLine and trailsEnabled.
func runAgentHelp(rootCmd *cobra.Command, args []string, repoLine string, asJSON, trailsEnabled bool) (string, error) {
	target := rootCmd
	for _, name := range args {
		child := agentHelpFindChild(target, name)
		if child == nil {
			return "", fmt.Errorf("unknown command %q; run `entire agent-help` for the list of commands", name)
		}
		// Keep the specific, actionable message for the trail-gated case.
		if !trailsEnabled && child.Annotations[agentHelpRequiresTrailsAnnotation] == agentHelpAnnotationEnabled {
			return "", fmt.Errorf("`%s` is unavailable: trails are not enabled for this repo", child.Name())
		}
		// The drillable surface must match the advertised surface: a name an agent
		// guesses for a command the listing intentionally hides (help, deprecated,
		// or plain-hidden infra like `hooks`) reads as nonexistent here too.
		if !isAgentHelpAdvertised(child, trailsEnabled) {
			return "", fmt.Errorf("unknown command %q; run `entire agent-help` for the list of commands", name)
		}
		target = child
	}
	if asJSON {
		return renderAgentHelpJSON(rootCmd, target, repoLine, trailsEnabled)
	}
	if target == rootCmd {
		return renderAgentHelpTop(rootCmd, repoLine, trailsEnabled), nil
	}
	return renderAgentHelpCommand(target, repoLine, trailsEnabled), nil
}

// agentHelpFindChild finds a direct child of parent by name or alias. It
// includes hidden commands so an annotated one like trail resolves; the caller
// (runAgentHelp) then enforces isAgentHelpAdvertised, so the drillable surface
// matches the advertised one.
func agentHelpFindChild(parent *cobra.Command, name string) *cobra.Command {
	for _, sub := range parent.Commands() {
		if sub.Name() == name {
			return sub
		}
		for _, alias := range sub.Aliases {
			if alias == name {
				return sub
			}
		}
	}
	return nil
}

type agentHelpFlagJSON struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage"`
}

type agentHelpSubcommandJSON struct {
	Name  string `json:"name"`
	Short string `json:"short"`
	// Audience mirrors the text renderer's grouping so a --json consumer gets the
	// same "may I run this unprompted?" answer as a text reader. Omitted when the
	// table makes no explicit claim, rather than asserting the user-owned default.
	Audience string `json:"audience,omitempty"`
}

type agentHelpJSON struct {
	Command     string                    `json:"command"`
	Short       string                    `json:"short,omitempty"`
	Long        string                    `json:"long,omitempty"`
	Example     string                    `json:"example,omitempty"`
	Repo        string                    `json:"repo,omitempty"`
	Flags       []agentHelpFlagJSON       `json:"flags,omitempty"`
	Subcommands []agentHelpSubcommandJSON `json:"subcommands,omitempty"`
}

// renderAgentHelpJSON renders the structured form of a command node.
func renderAgentHelpJSON(rootCmd, target *cobra.Command, repoLine string, trailsEnabled bool) (string, error) {
	doc := agentHelpJSON{
		Command: target.CommandPath(),
		Short:   target.Short,
		Long:    strings.TrimSpace(target.Long),
		Example: strings.TrimSpace(target.Example),
		Repo:    repoLine,
	}
	if target != rootCmd {
		collect := func(fs *flag.FlagSet) {
			fs.VisitAll(func(f *flag.Flag) {
				if f.Hidden {
					return
				}
				doc.Flags = append(doc.Flags, agentHelpFlagJSON{
					Name:      f.Name,
					Shorthand: f.Shorthand,
					Type:      f.Value.Type(),
					Default:   f.DefValue,
					Usage:     f.Usage,
				})
			})
		}
		collect(target.LocalFlags())
		collect(target.InheritedFlags())
	}
	for _, sub := range agentHelpCommands(target, trailsEnabled) {
		entry := agentHelpSubcommandJSON{Name: sub.Name(), Short: sub.Short}
		if a, ok := agentHelpClassified(agentHelpPath(sub)); ok {
			entry.Audience = agentHelpAudienceSlug(a)
		}
		doc.Subcommands = append(doc.Subcommands, entry)
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal agent-help json: %w", err)
	}
	return string(b) + "\n", nil
}

// isAgentHelpAdvertised reports whether sub should be exposed to agents through
// agent-help. The listing AND the drill-down resolver share this predicate so
// the drillable surface always matches the advertised surface: visible commands
// plus hidden commands that opt in via agentHelpAnnotation, minus the help
// command, deprecated commands, and (when trails are disabled) trail-gated ones.
func isAgentHelpAdvertised(sub *cobra.Command, trailsEnabled bool) bool {
	if sub.Name() == "help" || sub.Name() == "agent-help" || sub.Deprecated != "" {
		return false
	}
	if sub.Hidden && sub.Annotations[agentHelpAnnotation] != agentHelpAnnotationEnabled {
		return false
	}
	if !trailsEnabled && sub.Annotations[agentHelpRequiresTrailsAnnotation] == agentHelpAnnotationEnabled {
		return false
	}
	return true
}

// agentHelpCommands returns the child commands to advertise to agents.
func agentHelpCommands(parent *cobra.Command, trailsEnabled bool) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range parent.Commands() {
		if isAgentHelpAdvertised(sub, trailsEnabled) {
			out = append(out, sub)
		}
	}
	return out
}

// agentHelpRepoBlock formats the auto-detected repo line, degrading gracefully
// when the repo can't be resolved (no origin / detached HEAD) rather than
// implying a repo that isn't there.
func agentHelpRepoBlock(repoLine string) string {
	// Defense-in-depth: this line is emitted as plain text into agent context and
	// the user's terminal. A crafted origin URL's control characters (newline,
	// ANSI escapes) are rejected upstream in gitremote, but never let one reach
	// this plain-text sink — degrade to the not-detectable message instead.
	if strings.TrimSpace(repoLine) == "" || strings.IndexFunc(repoLine, unicode.IsControl) >= 0 {
		return "Current repo: not auto-detectable here (no origin remote / detached HEAD); pass --repo explicitly.\n"
	}
	return "Current repo: " + repoLine + "  (auto-detected from origin; pass --repo only for a DIFFERENT repo)\n"
}

// renderAgentHelpCommand renders one resolved command node for an agent: its
// path + Short, its Long description, the auto-detected repo line, its live flag
// usages (hidden flags are skipped by cobra), and its advertised subcommands.
func renderAgentHelpCommand(cmd *cobra.Command, repoLine string, trailsEnabled bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n", cmd.CommandPath(), cmd.Short)
	if long := strings.TrimSpace(cmd.Long); long != "" && long != strings.TrimSpace(cmd.Short) {
		b.WriteString(long)
		b.WriteString("\n")
	}
	if example := strings.TrimSpace(cmd.Example); example != "" {
		b.WriteString("\nExamples:\n")
		b.WriteString(example)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(agentHelpRepoBlock(repoLine))

	// LocalFlags()/InheritedFlags() trigger cobra's persistent-flag merge (plain
	// Flags() does not without Execute) and skip hidden flags in FlagUsages.
	if usages := strings.TrimRight(cmd.LocalFlags().FlagUsages(), "\n"); usages != "" {
		b.WriteString("\nFlags:\n")
		b.WriteString(usages)
		b.WriteString("\n")
	}
	if usages := strings.TrimRight(cmd.InheritedFlags().FlagUsages(), "\n"); usages != "" {
		b.WriteString("\nInherited flags:\n")
		b.WriteString(usages)
		b.WriteString("\n")
	}

	if subs := agentHelpCommands(cmd, trailsEnabled); len(subs) > 0 {
		names := make([]string, 0, len(subs))
		for _, sub := range subs {
			names = append(names, sub.Name())
		}
		fmt.Fprintf(&b, "\nSubcommands: %s\n", strings.Join(names, " · "))
		fmt.Fprintf(&b, "Next:  entire agent-help %s <subcommand>\n", strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" "))
	}
	return b.String()
}

// renderAgentHelpTop renders the top-level agent-facing overview: the curated
// intro + rule, the auto-detected repo line, and a live map of the advertised
// commands (their Short help), ending with the drill-down pointer.
func renderAgentHelpTop(rootCmd *cobra.Command, repoLine string, trailsEnabled bool) string {
	var b strings.Builder
	b.WriteString(agentHelpOverview)
	b.WriteString("\n\n")
	b.WriteString(agentHelpRepoBlock(repoLine))

	// Group by initiator rather than listing alphabetically: the agent's question
	// is "may I run this unprompted?", and only the grouping answers it.
	entries := agentHelpEntries(rootCmd, trailsEnabled)
	byAudience := map[agentHelpAudience][]agentHelpEntry{}
	width := 12
	for _, e := range entries {
		byAudience[e.audience] = append(byAudience[e.audience], e)
		// Broken-out subcommand paths are longer than bare names, so size the
		// column to the content instead of truncating into the Short help.
		if len(e.path) > width {
			width = len(e.path)
		}
	}
	b.WriteString("\nWhen to use entire:\n")
	for _, section := range agentHelpAudienceSections {
		bucket := byAudience[section.audience]
		if len(bucket) == 0 {
			// Experimental commands are absent from stable builds, so a whole
			// section can legitimately be empty — skip it rather than print a
			// heading over nothing.
			continue
		}
		fmt.Fprintf(&b, "\n  %s\n", section.heading)
		for _, e := range bucket {
			fmt.Fprintf(&b, "    %-*s %s\n", width, e.path, e.short)
		}
	}
	// Use an example command that is actually advertised here (trail is gated on
	// trails being enabled), so we never point at a command the agent can't use.
	example := "checkpoint"
	if trailsEnabled {
		example = "trail"
	}
	fmt.Fprintf(&b, "\nDrill in for exact, currently-installed flags:  entire agent-help <command>  (e.g. entire agent-help %s)\n", example)
	b.WriteString("Add --json for structured output.\n")
	return b.String()
}
