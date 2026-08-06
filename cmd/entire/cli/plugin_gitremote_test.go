package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// runGitIsolated runs a git command with the same config isolation
// testutil.CreateBranch/GitReset use. Without it these shell-outs inherit the
// developer's global git config — a `tag.gpgSign = true` there fails the tag
// calls below on their machine but not in CI.
func runGitIsolated(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Env = testutil.GitIsolatedEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// gitTagRepo tags a repo given its dir. Three separate copies of this
// shell-out existed across the plugin tests, none of them isolated.
func gitTagRepo(t *testing.T, dir, tag string) {
	t.Helper()
	runGitIsolated(t, "-C", dir, "tag", tag)
}

// repoDirFromURL maps a file:// fixture URL back to its directory.
func repoDirFromURL(repoURL string) string {
	return strings.TrimPrefix(repoURL, "file://")
}

// newTaggedPluginRepo creates a git repo with entire-plugin.yml (when
// metadata is non-empty) and the given tags, returning its file:// URL.
// file:// (not a bare path) forces git's transport machinery, matching how
// a real remote behaves for ls-remote and shallow clones.
func newTaggedPluginRepo(t *testing.T, metadata string, tags ...string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	if metadata != "" {
		testutil.WriteFile(t, dir, pluginMetadataFileName, metadata)
		testutil.GitAdd(t, dir, pluginMetadataFileName)
	} else {
		testutil.WriteFile(t, dir, "README.md", "readme")
		testutil.GitAdd(t, dir, "README.md")
	}
	testutil.GitCommit(t, dir, "init")
	for _, tag := range tags {
		gitTagRepo(t, dir, tag)
	}
	return "file://" + filepath.ToSlash(dir)
}

func TestListRemoteSemverTags(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "", "v0.1.0", "v0.10.0", "v0.2.0", "not-a-version", "1.0.0")
	tags, err := listRemoteSemverTags(context.Background(), url)
	if err != nil {
		t.Fatalf("listRemoteSemverTags: %v", err)
	}
	// Bare "1.0.0" is valid semver (canonicalized); "not-a-version" is dropped.
	want := []string{"1.0.0", "v0.10.0", "v0.2.0", "v0.1.0"}
	if len(tags) != len(want) {
		t.Fatalf("tags = %v, want %v", tags, want)
	}
	for i := range want {
		if tags[i] != want[i] {
			t.Errorf("tags[%d] = %q, want %q (full: %v)", i, tags[i], want[i], tags)
		}
	}
}

func TestListRemoteSemverTags_BadRemote(t *testing.T) {
	t.Parallel()
	if _, err := listRemoteSemverTags(context.Background(), "file:///nonexistent/repo"); err == nil {
		t.Error("listRemoteSemverTags succeeded on missing repo")
	}
}

func TestFetchPluginMetadataAtTag(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "name: demo\ndescription: a demo\n", "v1.0.0")
	meta, err := fetchPluginMetadataAtTag(context.Background(), url, "v1.0.0")
	if err != nil {
		t.Fatalf("fetchPluginMetadataAtTag: %v", err)
	}
	if meta == nil || meta.Name != "demo" {
		t.Errorf("meta = %+v, want name demo", meta)
	}
}

func TestFetchPluginMetadataAtTag_NoFileIsNilNil(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "", "v1.0.0")
	meta, err := fetchPluginMetadataAtTag(context.Background(), url, "v1.0.0")
	if err != nil {
		t.Fatalf("fetchPluginMetadataAtTag: %v", err)
	}
	if meta != nil {
		t.Errorf("meta = %+v, want nil for repo without %s", meta, pluginMetadataFileName)
	}
}

func TestValidatePluginRepoURL(t *testing.T) {
	t.Parallel()
	valid := []string{
		"https://github.com/entireio/entire-run",
		"ssh://git@github.com/entireio/entire-run.git",
		"file:///tmp/entire-run",
		"git@github.com:entireio/entire-run.git",
		"forge-user@git.example.com:team/entire-run",
	}
	for _, u := range valid {
		if err := validatePluginRepoURL(u); err != nil {
			t.Errorf("validatePluginRepoURL(%q) = %v, want nil", u, err)
		}
	}
	invalid := []string{
		"",
		"   ",
		"-x",
		// Unauthenticated, unencrypted transports: the catalog fetched over
		// them decides what installs without a prompt, so a network attacker
		// who can rewrite the transport chooses the binary.
		"http://forge.internal/team/entire-run.git",
		"git://example.com/entire-run.git",
		// Bare paths are rejected in favor of file://, which says the same
		// thing unambiguously — and keeps this validator identical to the
		// settings one.
		"/srv/plugin-index",
		`C:\\repos\\entire-run`,
		// The injection payloads: git reads an option-shaped positional as an
		// option, and --upload-pack's value is shell-interpreted.
		"--upload-pack=touch /tmp/pwned; git-upload-pack",
		"--config=core.pager=sh",
		// git's ext:: transport runs a command directly, so a "--" separator
		// alone would not stop it; the scheme allowlist does.
		"ext::sh -c whoami",
		"./relative/path",
		"../up/one",
		"entire-run",
		"https://",
	}
	for _, u := range invalid {
		if err := validatePluginRepoURL(u); err == nil {
			t.Errorf("validatePluginRepoURL(%q) = nil, want error", u)
		}
	}
}

// TestGitRemote_OptionShapedURLNeverExecutes is the regression test for
// argument injection into the git CLI. Repo URLs arrive from index.json
// entries and from other plugins' entire-plugin.yml requirements, so they are
// attacker-influenced. git parses an option-shaped positional as an option;
// `--upload-pack=<cmd>` is shell-interpreted and, with no positional
// repository left, runs against the *ambient* repo's origin. This asserts the
// stronger property — not just that the call fails, but that the payload
// never ran.
//
// No t.Parallel: t.Chdir mutates process-global state.
func TestGitRemote_OptionShapedURLNeverExecutes(t *testing.T) {
	remote := t.TempDir()
	runGitIsolated(t, "init", "-q", "--bare", remote)
	work := t.TempDir()
	testutil.InitRepo(t, work)
	testutil.WriteFile(t, work, "f.txt", "x")
	testutil.GitAdd(t, work, "f.txt")
	testutil.GitCommit(t, work, "init")
	runGitIsolated(t, "-C", work, "remote", "add", "origin", remote)
	t.Chdir(work)

	markerDir := t.TempDir()
	for _, tc := range []struct {
		name   string
		marker string
	}{
		{name: "ls-remote", marker: filepath.Join(markerDir, "tags-pwned")},
		{name: "clone", marker: filepath.Join(markerDir, "meta-pwned")},
	} {
		payload := "--upload-pack=touch " + tc.marker + "; git-upload-pack"
		var err error
		if tc.name == "ls-remote" {
			_, err = listRemoteSemverTags(context.Background(), payload)
		} else {
			_, err = fetchPluginMetadataAtTag(context.Background(), payload, "v1.0.0")
		}
		if err == nil {
			t.Errorf("%s accepted an option-shaped repo URL", tc.name)
		}
		if _, statErr := os.Stat(tc.marker); statErr == nil {
			t.Fatalf("%s: injected payload executed — %s was created", tc.name, tc.marker)
		}
	}
}

func TestPluginNameFromRepoURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url, want string
		wantErr   bool
	}{
		{url: "https://github.com/entireio/entire-run", want: "run"},
		{url: "https://github.com/entireio/entire-run.git", want: "run"},
		{url: "https://github.com/entireio/entire-run/", want: "run"},
		{url: "git@github.com:entireio/entire-brain.git", want: "brain"},
		{url: "https://github.com/entireio/some-tool", wantErr: true},
		{url: "https://github.com/entireio/entire-agent-x", wantErr: true}, // reserved
	}
	for _, tt := range tests {
		got, err := pluginNameFromRepoURL(tt.url)
		if tt.wantErr {
			if err == nil {
				t.Errorf("pluginNameFromRepoURL(%q) = %q, want error", tt.url, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("pluginNameFromRepoURL(%q) = %q, %v; want %q", tt.url, got, err, tt.want)
		}
	}
}

// semver ranks v2.0.0-rc1 above stable v1.9.0, so listing prereleases would
// migrate every user onto a release candidate on the next `plugin upgrade
// --all` the moment an author pushed one. --pin installs an exact tag and
// bypasses this listing, which is the deliberate opt-in.
func TestListRemoteSemverTags_SkipsPrereleases(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "", "v1.0.0", "v1.9.0", "v2.0.0-rc1", "v2.0.0-alpha", "v2.0.0+build7")
	tags, err := listRemoteSemverTags(context.Background(), url)
	if err != nil {
		t.Fatalf("listRemoteSemverTags: %v", err)
	}
	for _, tg := range tags {
		if strings.Contains(tg, "-rc") || strings.Contains(tg, "-alpha") {
			t.Errorf("prerelease %q was listed", tg)
		}
	}
	// Build metadata is not a prerelease and must survive.
	if len(tags) == 0 || tags[0] != "v2.0.0+build7" {
		t.Errorf("tags = %v, want v2.0.0+build7 newest", tags)
	}
}

// A repo with only prereleases must say so, not merely "no semver tags".
func TestListRemoteSemverTags_PrereleaseOnlyRepoIsEmpty(t *testing.T) {
	t.Parallel()
	url := newTaggedPluginRepo(t, "", "v1.0.0-rc1")
	tags, err := listRemoteSemverTags(context.Background(), url)
	if err != nil {
		t.Fatalf("listRemoteSemverTags: %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("tags = %v, want none", tags)
	}
}
