package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"

	"charm.land/huh/v2"
	"github.com/entireio/cli/cmd/entire/cli/interactive"
	"github.com/spf13/cobra"
)

// newPluginGroupCmd builds `entire plugin` and its subcommands. The kubectl
// dispatcher in plugin.go is the runtime mechanism — these commands manage a
// per-user managed directory that the dispatcher discovers because main.go
// prepends it to PATH at startup.
func newPluginGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage Entire plugins (install, list, upgrade, search, remove)",
		Long: `Manage Entire plugins.

Plugins are external executables named 'entire-<name>'. The CLI discovers
plugins on $PATH and from a per-user managed directory which is
auto-prepended to PATH at startup. The managed directory is, in order of
precedence:

  $ENTIRE_PLUGIN_DIR/bin (override)
  $XDG_DATA_HOME/entire/plugins/bin (Linux/macOS, when set)
  ~/.local/share/entire/plugins/bin (Linux/macOS default)
  %LOCALAPPDATA%\entire\plugins\bin (Windows, when set)
  ~\AppData\Local\entire\plugins\bin (Windows fallback when LOCALAPPDATA is unset)

Install sources:
  entire plugin install run                                    index lookup
  entire plugin install https://github.com/entireio/entire-run repository URL
  entire plugin install ./dist/entire-run                      local executable

Remote installs resolve the newest semver tag over the git protocol, then
download the platform's release asset (verified against the release's
checksums.txt when published). Discovery uses a git-synced plugin index;
see 'entire plugin search' and 'entire plugin index update'.`,
	}

	cmd.AddCommand(newPluginInstallCmd())
	cmd.AddCommand(newPluginListCmd())
	cmd.AddCommand(newPluginRemoveCmd())
	cmd.AddCommand(newPluginUpgradeCmd())
	cmd.AddCommand(newPluginSearchCmd())
	cmd.AddCommand(newPluginInfoCmd())
	cmd.AddCommand(newPluginBrowseCmd())
	cmd.AddCommand(newPluginDoctorCmd())
	cmd.AddCommand(newPluginIndexCmd())
	return cmd
}

// installArgKind classifies the install argument.
type installArgKind int

const (
	installFromPath installArgKind = iota
	installFromURL
	installFromIndex
)

// classifyInstallArg distinguishes the three install sources. URLs are
// anything with a scheme or scp-like git@ prefix; paths must be explicit —
// a separator or a leading dot (./entire-foo) — and everything else is a
// bare name for index lookup. Deliberately NOT stat-based: a stray file or
// directory in the CWD sharing a plugin's name must not shadow the index
// (and could never install anyway — path installs require an entire-
// basename). The spaces stay disjoint because validatePluginName rejects
// separators in plugin names.
func classifyInstallArg(arg string) installArgKind {
	if strings.Contains(arg, "://") || strings.HasPrefix(arg, "git@") {
		return installFromURL
	}
	if strings.ContainsAny(arg, `/\`) || strings.HasPrefix(arg, ".") {
		return installFromPath
	}
	return installFromIndex
}

func newPluginInstallCmd() *cobra.Command {
	var force, yes, noDeps bool
	var pin, indexFlag string
	cmd := &cobra.Command{
		Use:   "install <name|url|path>",
		Short: "Install a plugin from the index, a git repository URL, or a local executable",
		Long: `Install a plugin.

Three source forms:

  name   Bare names resolve through the plugin index:
             entire plugin install run
  url    Full git repository URLs install from any git host. The newest
         semver tag is resolved with 'git ls-remote'; the platform's release
         asset is downloaded and verified against the release's
         checksums.txt when one is published:
             entire plugin install https://github.com/entireio/entire-run
  path   Local executables are linked into the managed directory (symlink
         first, so rebuilds are picked up immediately). Paths must be
         explicit — a separator or leading ./ — so a stray local file can
         never shadow an index name:
             entire plugin install ./dist/entire-run
             entire plugin install ./entire-run

Installing from a URL that is not listed in the plugin index asks for
confirmation; pass --yes to skip (required in non-interactive runs).

Plugins may declare dependencies in entire-plugin.yml. Missing dependencies
are listed and installed after a single confirmation (or with --yes);
--no-deps opts out.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			arg := args[0]

			if classifyInstallArg(arg) == installFromPath {
				p, err := InstallPluginFromPath(InstallPluginOptions{SourcePath: arg, Force: force})
				if err != nil {
					return fmt.Errorf("install plugin: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Installed plugin %q → %s\n", p.Name, p.Path)
				warnIfShadowsBuiltin(cmd, p.Name)
				return nil
			}
			return silencePluginCancel(ctx, runRemoteInstall(ctx, cmd, arg, remoteInstallFlags{
				force: force, yes: yes, noDeps: noDeps, pin: pin, index: indexFlag,
			}))
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Replace an existing entry with the same name")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompts (non-index sources, dependency installs)")
	cmd.Flags().BoolVar(&noDeps, "no-deps", false, "Do not install declared dependencies")
	cmd.Flags().StringVar(&pin, "pin", "", "Install exactly this tag and skip it during 'plugin upgrade'")
	cmd.Flags().StringVar(&indexFlag, "index", "", "Plugin index URL (overrides settings and "+pluginIndexEnvVar+")")
	return cmd
}

type remoteInstallFlags struct {
	force, yes, noDeps bool
	pin, index         string
}

func runRemoteInstall(ctx context.Context, cmd *cobra.Command, arg string, flags remoteInstallFlags) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	repoURL := arg
	var trusted bool
	var idx *PluginIndex

	if classifyInstallArg(arg) == installFromIndex {
		var err error
		idx, err = SyncPluginIndex(ctx, resolvePluginIndexURL(ctx, flags.index), false)
		if err != nil {
			return fmt.Errorf("resolve %q via plugin index: %w", arg, err)
		}
		entry := idx.Find(arg)
		if entry == nil {
			// Bare names never resolve to local files (see
			// classifyInstallArg), but a user who typed one expecting a
			// path install deserves the pointer.
			if _, statErr := os.Stat(arg); statErr == nil {
				return fmt.Errorf("plugin %q is not in the index; to install the local file, use an explicit path: entire plugin install ./%s", arg, arg)
			}
			return fmt.Errorf("plugin %q is not in the index; pass the repository URL to install from a specific repo (try 'entire plugin search %s')", arg, arg)
		}
		if len(entry.Platforms) > 0 && !slices.Contains(entry.Platforms, runtime.GOOS) {
			fmt.Fprintf(errOut, "Warning: index lists %q for %s only; this is %s — continuing anyway.\n",
				arg, strings.Join(entry.Platforms, "/"), runtime.GOOS)
		}
		repoURL = entry.RepoURL
		trusted = true
	} else {
		// URL install: the index is only consulted for the trust check.
		// An unreachable index degrades to "not listed" rather than
		// blocking the install.
		var idxErr error
		idx, idxErr = SyncPluginIndex(ctx, resolvePluginIndexURL(ctx, flags.index), false)
		trusted = idxErr == nil && idx.HasRepoURL(repoURL)
	}

	if !trusted {
		ok, err := confirmPluginAction(ctx,
			fmt.Sprintf("Install from %s? The repository is not listed in the plugin index.", repoURL),
			flags.yes)
		switch {
		case errors.Is(err, errConfirmNeedsTerminal):
			return err // untrusted source can't proceed unconfirmed
		case err != nil:
			// Ctrl+C/Esc in the prompt prints "Install cancelled." and
			// exits cleanly; real prompt failures surface wrapped.
			return handleFormCancellation(out, "Install", err)
		case !ok:
			// Same outcome as an abort: nothing installed, clean exit.
			// Exit codes must not differ between Esc and answering "No" —
			// and automation never reaches this prompt at all (the
			// non-interactive path fails above with the --yes hint).
			fmt.Fprintln(out, "Install cancelled.")
			return nil
		}
	}

	res, err := InstallPluginFromRepo(ctx, RemoteInstallOptions{RepoURL: repoURL, Pin: flags.pin, Force: flags.force})
	if err != nil {
		return fmt.Errorf("install plugin: %w", err)
	}
	for _, t := range res.SkippedTags {
		fmt.Fprintf(errOut, "Warning: tag %s has no release asset for this platform; fell back to %s.\n", t, res.Manifest.Tag)
	}
	fmt.Fprintf(out, "Installed plugin %q %s from %s\n", res.Manifest.Name, res.Manifest.Tag, repoURL)
	warnIfShadowsBuiltin(cmd, res.Manifest.Name)

	if flags.noDeps || res.Metadata == nil || len(res.Metadata.Requires) == 0 {
		return nil
	}
	return installPlannedDeps(ctx, cmd, res.Metadata.Requires, idx, flags.yes)
}

// installPlannedDeps plans, confirms once (apt-style), and executes
// dependency installs. The main plugin is already installed at this point,
// so a declined or non-confirmable plan degrades to a warning, not an
// error — doctor reports the gap afterwards.
func installPlannedDeps(ctx context.Context, cmd *cobra.Command, reqs []PluginRequirement, idx *PluginIndex, yes bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	plan, err := PlanDependencyInstalls(ctx, reqs, idx)
	if err != nil {
		return fmt.Errorf("resolve dependencies: %w", err)
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(errOut, "Warning: %s\n", w)
	}
	if len(plan.Actions) == 0 {
		return nil
	}

	fmt.Fprintf(out, "\nThis plugin requires %d additional plugin(s):\n", len(plan.Actions))
	for _, a := range plan.Actions {
		switch {
		case a.Upgrade:
			fmt.Fprintf(out, "  %s  (installed %s, needs >= %s — will upgrade)\n", a.Name, a.CurrentTag, a.MinVersion)
		default:
			fmt.Fprintf(out, "  %s  (%s)\n", a.Name, a.RepoURL)
		}
	}
	ok, err := confirmPluginAction(ctx, "Install them now?", yes)
	switch {
	case errors.Is(err, errConfirmNeedsTerminal):
		// Non-interactive without --yes: the main install already
		// succeeded, so skip with a pointer instead of failing late.
		fmt.Fprintln(errOut, "Skipping dependency installs (no terminal for confirmation; re-run with --yes). 'entire plugin doctor' will report what's missing.")
		return nil
	case err != nil:
		// User abort prints "Dependency install cancelled." and falls
		// through to the skip note; real prompt failures are returned —
		// claiming "skipped" for an error the user never saw would be
		// misreporting.
		if cancelErr := handleFormCancellation(errOut, "Dependency install", err); cancelErr != nil {
			return cancelErr
		}
		fmt.Fprintln(errOut, "'entire plugin doctor' will report what's missing.")
		return nil
	case !ok:
		fmt.Fprintln(errOut, "Skipping dependency installs; 'entire plugin doctor' will report what's missing.")
		return nil
	}
	if err := ExecuteDepPlan(ctx, plan); err != nil {
		return err
	}
	for _, a := range plan.Actions {
		fmt.Fprintf(out, "Installed dependency %q\n", a.Name)
	}
	return nil
}

// errConfirmNeedsTerminal signals that a confirmation was required but no
// terminal is available and --yes was not passed. Callers decide whether
// that is fatal (untrusted install) or an informed skip (dependency
// installs after the main install already succeeded).
var errConfirmNeedsTerminal = errors.New("confirmation required but no terminal available; re-run with --yes")

// confirmPluginAction asks a yes/no question. assumeYes short-circuits;
// non-interactive runs without --yes return errConfirmNeedsTerminal rather
// than guessing. Prompt errors (including huh.ErrUserAborted on Ctrl+C/Esc)
// are returned raw for callers to map via handleFormCancellation.
func confirmPluginAction(ctx context.Context, prompt string, assumeYes bool) (bool, error) {
	if assumeYes {
		return true, nil
	}
	if !interactive.CanPromptInteractively() {
		return false, fmt.Errorf("%w (%s)", errConfirmNeedsTerminal, prompt)
	}
	confirmed := false
	form := NewAccessibleForm(huh.NewGroup(
		huh.NewConfirm().Title(prompt).Value(&confirmed),
	))
	if err := form.RunWithContext(ctx); err != nil {
		// %w keeps huh.ErrUserAborted reachable for handleFormCancellation.
		return false, fmt.Errorf("confirm: %w", err)
	}
	return confirmed, nil
}

// silencePluginCancel maps Ctrl+C-induced failures to a SilentError per the
// codebase convention (clean.go, activity_cmd.go) — printing "context
// canceled" at a user who just interrupted a clone or download is noise.
// The ctx.Err() check matters because a killed git child surfaces as
// "signal: killed", not context.Canceled, when the cancellation raced the
// subprocess.
func silencePluginCancel(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewSilentError(err)
	}
	return err
}

// warnIfShadowsBuiltin prints a one-line note to stderr when the just-installed
// plugin name matches a built-in command. The dispatcher's resolvePlugin gates
// dispatch on rootCmd.Find, so the built-in always wins at runtime — without
// this hint, a user who installed a shadowed plugin would silently get the
// built-in and have no idea their install was inert. We mirror the dispatcher's
// help/completion priming so names like "help" surface the warning too.
func warnIfShadowsBuiltin(cmd *cobra.Command, name string) {
	root := cmd.Root()
	if root == nil {
		return
	}
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd(name)
	if c, _, err := root.Find([]string{name}); err == nil && c != root {
		fmt.Fprintf(cmd.ErrOrStderr(), "Note: %q shadows the built-in command; the built-in will take precedence at runtime.\n", name)
	}
}

func newPluginListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List plugins installed in the managed directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPluginList(cmd.OutOrStdout())
		},
	}
}

func runPluginList(w io.Writer) error {
	plugins, err := ListInstalledPlugins()
	if err != nil {
		return fmt.Errorf("list plugins: %w", err)
	}
	dir, err := PluginBinDir()
	if err != nil {
		return fmt.Errorf("plugin bin dir: %w", err)
	}
	if len(plugins) == 0 {
		fmt.Fprintf(w, "No plugins installed in %s.\n", dir)
		fmt.Fprintln(w, "Install one with 'entire plugin install <name|url|path>', or drop an entire-<name> binary anywhere on $PATH.")
		return nil
	}
	manifestTag := map[string]string{}
	if manifests, err := ListPluginManifests(); err == nil {
		for _, m := range manifests {
			tag := m.Tag
			if m.Pinned {
				tag += " (pinned)"
			}
			manifestTag[m.Name] = tag
		}
	}
	fmt.Fprintf(w, "Managed plugin directory: %s\n\n", dir)
	for _, p := range plugins {
		tag := manifestTag[p.Name]
		if p.Symlink {
			fmt.Fprintf(w, "  %-20s %-18s → %s\n", p.Name, tag, p.LinkTarget)
		} else {
			fmt.Fprintf(w, "  %-20s %-18s %s\n", p.Name, tag, p.Path)
		}
	}
	return nil
}

func newPluginRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a plugin from the managed directory",
		Long: `Remove a plugin from the managed directory.

Only entries in the managed directory are affected. Plugins installed by
dropping a binary elsewhere on $PATH are unmanaged — remove those by hand.

When other installed plugins declare the target as a dependency, removal
is refused unless --force is given.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !force {
				dependents, err := DependentsOf(name)
				if err != nil {
					return err
				}
				if len(dependents) > 0 {
					return fmt.Errorf("plugin %q is required by %s; use --force to remove anyway", name, strings.Join(dependents, ", "))
				}
			}
			if err := RemoveManagedPlugin(name); err != nil {
				return fmt.Errorf("remove plugin: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed plugin %q\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Remove even when other plugins depend on it")
	return cmd
}

func newPluginUpgradeCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "upgrade [name]",
		Short: "Upgrade remote-installed plugins to their newest tag",
		Long: `Upgrade remote-installed plugins to their newest semver tag.

Only plugins installed from a repository URL or the index carry the install
manifest upgrades need; local-dev symlink installs are skipped. Plugins
installed with --pin are skipped until reinstalled without the pin.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			var names []string
			switch {
			case len(args) == 1:
				names = []string{args[0]}
			case all:
				manifests, err := ListPluginManifests()
				if err != nil {
					return err
				}
				for _, m := range manifests {
					names = append(names, m.Name)
				}
				if len(names) == 0 {
					fmt.Fprintln(out, "No upgradable plugins (none were installed from a repository).")
					return nil
				}
			default:
				return errors.New("specify a plugin name or --all")
			}
			var firstErr error
			for _, name := range names {
				o, err := UpgradeInstalledPlugin(ctx, name)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Upgrade %q failed: %v\n", name, err)
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				switch {
				case o.Pinned:
					fmt.Fprintf(out, "%-20s pinned, skipped\n", name)
				case o.UpToDate:
					fmt.Fprintf(out, "%-20s up to date\n", name)
				default:
					fmt.Fprintf(out, "%-20s %s → %s\n", name, o.FromTag, o.ToTag)
				}
			}
			return silencePluginCancel(ctx, firstErr)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Upgrade every remote-installed plugin")
	return cmd
}

func newPluginSearchCmd() *cobra.Command {
	var indexFlag string
	cmd := &cobra.Command{
		Use:   "search [term]",
		Short: "Search the plugin index",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			term := ""
			if len(args) == 1 {
				term = args[0]
			}
			idx, err := SyncPluginIndex(ctx, resolvePluginIndexURL(ctx, indexFlag), false)
			if err != nil {
				return silencePluginCancel(ctx, err)
			}
			entries := idx.Search(term)
			if len(entries) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No plugins matching %q in the index.\n", term)
				return nil
			}
			printIndexEntries(cmd.OutOrStdout(), entries)
			return nil
		},
	}
	cmd.Flags().StringVar(&indexFlag, "index", "", "Plugin index URL (overrides settings and "+pluginIndexEnvVar+")")
	return cmd
}

func printIndexEntries(w io.Writer, entries []PluginIndexEntry) {
	installedNames := map[string]bool{}
	if installed, err := ListInstalledPlugins(); err == nil {
		for _, p := range installed {
			installedNames[p.Name] = true
		}
	}
	for _, e := range entries {
		mark := " "
		if installedNames[e.Name] {
			mark = "*"
		}
		official := ""
		if e.Official {
			official = " [official]"
		}
		fmt.Fprintf(w, "%s %-20s %s%s\n", mark, e.Name, e.Description, official)
	}
	fmt.Fprintln(w, "\n* = installed. Install with 'entire plugin install <name>'.")
}

func newPluginInfoCmd() *cobra.Command {
	var indexFlag string
	cmd := &cobra.Command{
		Use:   "info <name>",
		Short: "Show index and install details for a plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			out := cmd.OutOrStdout()

			entry := (*PluginIndexEntry)(nil)
			if idx, err := SyncPluginIndex(ctx, resolvePluginIndexURL(ctx, indexFlag), false); err == nil {
				entry = idx.Find(name)
			}
			m, err := LoadPluginManifest(name)
			if err != nil {
				return err
			}
			installed, err := FindInstalledPlugin(name)
			if err != nil {
				return err
			}
			if entry == nil && m == nil && installed == nil {
				return fmt.Errorf("plugin %q: not installed and not in the index", name)
			}

			fmt.Fprintf(out, "Name:        %s\n", name)
			if entry != nil {
				fmt.Fprintf(out, "Description: %s\n", entry.Description)
				fmt.Fprintf(out, "Repository:  %s\n", entry.RepoURL)
				fmt.Fprintf(out, "Official:    %t\n", entry.Official)
				if len(entry.Platforms) > 0 {
					fmt.Fprintf(out, "Platforms:   %s\n", strings.Join(entry.Platforms, ", "))
				}
			}
			switch {
			case m != nil:
				fmt.Fprintf(out, "Installed:   %s (from %s", m.Tag, m.RepoURL)
				if m.Pinned {
					fmt.Fprint(out, ", pinned")
				}
				fmt.Fprintln(out, ")")
				for _, r := range m.Requires {
					line := "Requires:    " + r.Name
					if r.MinVersion != "" {
						line += " >= " + r.MinVersion
					}
					fmt.Fprintln(out, line)
				}
			case installed != nil:
				fmt.Fprintf(out, "Installed:   local (%s)\n", installed.Path)
			default:
				fmt.Fprintln(out, "Installed:   no")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&indexFlag, "index", "", "Plugin index URL (overrides settings and "+pluginIndexEnvVar+")")
	return cmd
}

func newPluginBrowseCmd() *cobra.Command {
	var indexFlag string
	cmd := &cobra.Command{
		Use:   "browse",
		Short: "Interactively browse the plugin index and install",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if !interactive.CanPromptInteractively() {
				return errors.New("browse needs a terminal; use 'entire plugin search' instead")
			}
			idx, err := SyncPluginIndex(ctx, resolvePluginIndexURL(ctx, indexFlag), false)
			if err != nil {
				return silencePluginCancel(ctx, err)
			}
			if len(idx.Plugins) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "The plugin index is empty.")
				return nil
			}
			options := make([]huh.Option[string], 0, len(idx.Plugins)+1)
			for _, e := range idx.Plugins {
				label := e.Name
				if e.Description != "" {
					label = fmt.Sprintf("%s — %s", e.Name, e.Description)
				}
				options = append(options, huh.NewOption(label, e.Name))
			}
			options = append(options, huh.NewOption("(cancel)", ""))
			choice := ""
			form := NewAccessibleForm(huh.NewGroup(
				huh.NewSelect[string]().Title("Install a plugin").Options(options...).Value(&choice),
			))
			if err := form.RunWithContext(ctx); err != nil {
				return handleFormCancellation(cmd.OutOrStdout(), "Browse", err)
			}
			if choice == "" {
				return nil
			}
			return silencePluginCancel(ctx, runRemoteInstall(ctx, cmd, choice, remoteInstallFlags{index: indexFlag}))
		},
	}
	cmd.Flags().StringVar(&indexFlag, "index", "", "Plugin index URL (overrides settings and "+pluginIndexEnvVar+")")
	return cmd
}

func newPluginDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check installed plugins for missing dependencies and broken entries",
		RunE: func(cmd *cobra.Command, _ []string) error {
			issues, err := RunPluginDoctor(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(issues) == 0 {
				fmt.Fprintln(out, "All plugins healthy.")
				return nil
			}
			for _, i := range issues {
				fmt.Fprintf(out, "%s: %s\n", i.Plugin, i.Problem)
				if i.Fix != "" {
					fmt.Fprintf(out, "    fix: %s\n", i.Fix)
				}
			}
			cmd.SilenceUsage = true
			return NewSilentError(fmt.Errorf("%d plugin issue(s) found", len(issues)))
		},
	}
}

func newPluginIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Manage the plugin index",
	}
	var indexFlag string
	update := &cobra.Command{
		Use:   "update",
		Short: "Force a refresh of the plugin index",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			url := resolvePluginIndexURL(ctx, indexFlag)
			idx, err := SyncPluginIndex(ctx, url, true)
			if err != nil {
				return silencePluginCancel(ctx, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Index %s: %d plugin(s).\n", url, len(idx.Plugins))
			return nil
		},
	}
	update.Flags().StringVar(&indexFlag, "index", "", "Plugin index URL (overrides settings and "+pluginIndexEnvVar+")")
	cmd.AddCommand(update)
	return cmd
}
