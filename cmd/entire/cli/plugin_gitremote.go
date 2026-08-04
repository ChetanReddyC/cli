package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/semver"
)

// Host-agnostic remote inspection over the git protocol. Version listing
// uses `git ls-remote --tags`, which behaves identically on GitHub, GitLab,
// Gitea/Forgejo, and any self-hosted server, inherits the user's existing
// git auth (SSH agent, credential helpers), and needs no forge REST client.
// The shell-out (vs go-git) is deliberate: this path needs the user's
// credential helpers and proxy config, the same reason hooks.go shells out
// for network-touching operations.

// gitQuiet returns an exec.Cmd for git with terminal prompts disabled, so
// an auth-requiring remote fails fast instead of hanging a non-interactive
// run on a username prompt.
func gitQuiet(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// pluginRepoURLSchemes are the transports a plugin (or index) repository URL
// may use. Mirrors settings.PluginSettings.Validate's allowlist, plus the
// git:// and http:// transports the release-asset path already accepts.
var pluginRepoURLSchemes = []string{"https://", "http://", "ssh://", "git://", "file://"}

// scpLikeGitURL matches git's scp-like syntax (user@host:path) — the form
// `git@github.com:owner/repo.git` uses. Requires a user@, a host without a
// slash, and a colon before the path, so it can't match a bare option or a
// local path.
var scpLikeGitURL = regexp.MustCompile(`^[A-Za-z0-9._~-]+@[A-Za-z0-9._-]+:[^\s]`)

// validatePluginRepoURL rejects anything that isn't recognizably a git
// repository URL before it reaches the git CLI as a positional argument.
//
// This is a security boundary, not a nicety. Repo URLs arrive from
// attacker-influenced places — index.json entries, another plugin's
// entire-plugin.yml `requires`, a stored manifest — and git parses an
// option-shaped positional as an option. `--upload-pack=<cmd>` is
// shell-interpreted and runs against the *ambient* repo when no positional
// remains, so an entry like "--upload-pack=curl … | sh; git-upload-pack"
// would execute arbitrary commands during an index-resolved install, which
// never prompts. validatePluginName already refuses a leading '-' on names
// for the same reason; URLs need the same treatment.
//
// Callers additionally pass "--" before positionals (defense in depth), so
// an option-shaped value would be rejected by git even if it got this far.
func validatePluginRepoURL(rawURL string) error {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return errors.New("repository URL is empty")
	}
	if strings.HasPrefix(u, "-") {
		return fmt.Errorf("repository URL %q must not start with '-'", rawURL)
	}
	for _, scheme := range pluginRepoURLSchemes {
		if strings.HasPrefix(u, scheme) && len(u) > len(scheme) {
			return nil
		}
	}
	if scpLikeGitURL.MatchString(u) {
		return nil
	}
	// An absolute local path is a valid git remote and can't be read as an
	// option, so it stays accepted — `--index /srv/plugin-index` and
	// bare-path test fixtures keep working. Relative paths and bare names
	// are still rejected: they'd be ambiguous with plugin names.
	if isAbsoluteGitPath(u) {
		return nil
	}
	return fmt.Errorf("repository URL %q must be an absolute path or use one of %s or git's user@host:path form",
		rawURL, strings.Join(pluginRepoURLSchemes, ", "))
}

// isAbsoluteGitPath reports whether u is an absolute filesystem path: a
// leading separator, or a Windows drive prefix like C:\ or C:/.
func isAbsoluteGitPath(u string) bool {
	if strings.HasPrefix(u, "/") || strings.HasPrefix(u, `\\`) {
		return true
	}
	return len(u) >= 3 && u[1] == ':' &&
		(u[2] == '/' || u[2] == '\\') &&
		((u[0] >= 'A' && u[0] <= 'Z') || (u[0] >= 'a' && u[0] <= 'z'))
}

// listRemoteSemverTags returns the repo's semver tags ("v"-prefixed or
// bare), sorted descending by semantic version. Non-semver tags are
// ignored.
func listRemoteSemverTags(ctx context.Context, repoURL string) ([]string, error) {
	if err := validatePluginRepoURL(repoURL); err != nil {
		return nil, err
	}
	// --refs suppresses peeled ^{} entries for annotated tags. "--" keeps an
	// option-shaped URL from being parsed as a git option.
	out, err := gitQuiet(ctx, "ls-remote", "--tags", "--refs", "--", repoURL).Output()
	if err != nil {
		return nil, fmt.Errorf("list tags of %s: %w%s", repoURL, err, stderrSuffix(err))
	}
	var tags []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		tag := strings.TrimPrefix(fields[1], "refs/tags/")
		if semver.IsValid(canonicalSemver(tag)) {
			tags = append(tags, tag)
		}
	}
	sort.Slice(tags, func(i, j int) bool {
		return semver.Compare(canonicalSemver(tags[i]), canonicalSemver(tags[j])) > 0
	})
	return tags, nil
}

// canonicalSemver maps a tag to the "v"-prefixed form x/mod/semver expects.
func canonicalSemver(tag string) string {
	if strings.HasPrefix(tag, "v") {
		return tag
	}
	return "v" + tag
}

// stderrSuffix extracts captured stderr from an exec.ExitError for error
// messages; git's stderr is where the actionable detail lives. Only
// populated when the command was started via Output().
func stderrSuffix(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return ": " + strings.TrimSpace(string(exitErr.Stderr))
	}
	return ""
}

// fetchPluginMetadataAtTag reads entire-plugin.yml at the given tag without
// a user-visible clone: a blobless shallow clone into a discarded temp dir,
// falling back to a plain shallow clone for servers that don't allow
// partial-clone filters (uploadpack.allowFilter defaults to off outside the
// big forges). Returns (nil, nil) when the repo has no metadata file —
// metadata is optional by design.
func fetchPluginMetadataAtTag(ctx context.Context, repoURL, tag string) (*PluginMetadata, error) {
	if err := validatePluginRepoURL(repoURL); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "entire-plugin-meta-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	cloneArgs := func(filter bool) []string {
		args := []string{"clone", "--depth", "1", "--no-checkout", "--branch", tag, "--quiet"}
		if filter {
			args = append(args, "--filter=blob:none")
		}
		// "--" so an option-shaped URL can't be parsed as a git option.
		return append(args, "--", repoURL, tmp)
	}
	// Output() (not Run()) so stderr is captured into the ExitError for
	// error messages; clone --quiet writes nothing to stdout.
	if _, err := gitQuiet(ctx, cloneArgs(true)...).Output(); err != nil {
		// Retry without the filter; the clone dir may be half-created.
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			return nil, fmt.Errorf("clean temp clone dir: %w", rmErr)
		}
		if _, err := gitQuiet(ctx, cloneArgs(false)...).Output(); err != nil {
			return nil, fmt.Errorf("clone %s at %s: %w%s", repoURL, tag, err, stderrSuffix(err))
		}
	}
	// `git show <rev>:<path>` peels annotated tags and, in a blobless
	// clone, lazily fetches just this one blob from the promisor remote.
	out, err := gitQuiet(ctx, "-C", tmp, "show", tag+":"+pluginMetadataFileName).Output()
	if err != nil {
		// Missing file and missing tag both surface as exit 128; only the
		// missing-file case is benign. Disambiguate via stderr.
		if strings.Contains(stderrSuffix(err), "does not exist") ||
			strings.Contains(stderrSuffix(err), "exists on disk, but not in") {
			return nil, nil //nolint:nilnil // metadata is optional
		}
		return nil, fmt.Errorf("read %s at %s: %w%s", pluginMetadataFileName, tag, err, stderrSuffix(err))
	}
	return ParsePluginMetadata(out)
}

// pluginNameFromRepoURL derives the bare plugin name from a repository URL
// basename: .../entire-run(.git) → "run". Used when entire-plugin.yml is
// absent or doesn't declare a name.
func pluginNameFromRepoURL(repoURL string) (string, error) {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(repoURL, "/"), ".git")
	base := trimmed
	if i := strings.LastIndexAny(trimmed, "/:"); i >= 0 {
		base = trimmed[i+1:]
	}
	if !strings.HasPrefix(base, pluginBinaryPrefix) {
		return "", fmt.Errorf("repository basename %q does not start with %q; declare a name in %s", base, pluginBinaryPrefix, pluginMetadataFileName)
	}
	name := strings.TrimPrefix(base, pluginBinaryPrefix)
	if err := validatePluginName(name); err != nil {
		return "", err
	}
	return name, nil
}
