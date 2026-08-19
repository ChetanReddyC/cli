package settings

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

const (
	attackerCommand = "./.entire/opf"
	trustedCommand  = "/opt/opf/bin/opf"
)

// opfSettings renders a settings file that enables OPF, optionally with an
// explicit command.
func opfSettings(command string) string {
	body := `{"enabled":true,"redaction":{"openai_privacy_filter":{` +
		`"enabled":true,"prompt_default":"always","categories":{"private_person":true}`
	if command != "" {
		body += `,"command":"` + command + `"`
	}
	return body + `}}}`
}

// localOPFSettings renders a local override file that sets only the command.
func localOPFSettings(command string) string {
	return `{"redaction":{"openai_privacy_filter":{"command":"` + command + `"}}}`
}

// newOPFRepo creates an initialized repo with .entire/ and returns the repo
// root plus the absolute project and local settings paths.
func newOPFRepo(t *testing.T) (root, projectPath, localPath string) {
	t.Helper()
	root = t.TempDir()
	testutil.InitRepo(t, root)
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
	// Mirror the shipped .entire/.gitignore so the force-add cases prove what
	// they claim: ignoring a path does not protect it once it is tracked.
	require.NoError(t, os.WriteFile(filepath.Join(root, ".entire", ".gitignore"),
		[]byte("settings.local.json\n"), 0o644))
	return root, filepath.Join(root, EntireSettingsFile), filepath.Join(root, EntireSettingsLocalFile)
}

func writeSettingsFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

// loadedOPF runs the real merge path and returns the effective OPF block.
func loadedOPF(t *testing.T, projectPath, localPath string) *OPFSettings {
	t.Helper()
	s, err := loadMergedSettings(t.Context(), projectPath, "", localPath)
	require.NoError(t, err)
	require.NotNil(t, s.Redaction)
	require.NotNil(t, s.Redaction.OpenAIPrivacyFilter)
	return s.Redaction.OpenAIPrivacyFilter
}

// A command in the version-controlled project file is attacker-deliverable
// through an ordinary pull request, so it must never reach the exec path.
func TestOPFCommandTrust_ProjectSettingsCommandIsIgnored(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(attackerCommand))

	opf := loadedOPF(t, project, local)
	assert.Empty(t, opf.Command, "command from the committed project settings file must be dropped")

	cmd, reason, rejected := opf.CommandRejection()
	assert.True(t, rejected, "the rejection must be reportable to the consumer")
	assert.Equal(t, attackerCommand, cmd, "the ignored command is preserved for the warning")
	assert.Contains(t, reason, "settings.local.json", "the reason names where the command must live")
}

// The supported configuration: developer-owned, untracked local override.
func TestOPFCommandTrust_UntrackedLocalCommandIsHonored(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local, localOPFSettings(trustedCommand))

	opf := loadedOPF(t, project, local)
	assert.Equal(t, trustedCommand, opf.Command,
		"an untracked local override is developer-owned and must be honored")
	_, _, rejected := opf.CommandRejection()
	assert.False(t, rejected, "a trusted command must not be reported as rejected")
}

// The filename is not the boundary: .gitignore does not apply to an
// already-tracked path, so `git add -f` makes the local file PR-deliverable.
func TestOPFCommandTrust_StagedLocalFileIsIgnored(t *testing.T) {
	t.Parallel()
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local, localOPFSettings(attackerCommand))

	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)

	assert.Empty(t, loadedOPF(t, project, local).Command,
		"a local file tracked in the index must not be trusted")
}

// Committed in HEAD is the shape a pull request actually delivers.
func TestOPFCommandTrust_CommittedLocalFileIsIgnored(t *testing.T) {
	t.Parallel()
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local, localOPFSettings(attackerCommand))

	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, root, "commit", "-m", "carry local settings")

	assert.Empty(t, loadedOPF(t, project, local).Command,
		"a local file present in HEAD must not be trusted")
}

// Removing the file from the index after committing still leaves the content
// reachable in HEAD, so the index check alone is not sufficient.
func TestOPFCommandTrust_CommittedThenUnstagedIsIgnored(t *testing.T) {
	t.Parallel()
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local, localOPFSettings(attackerCommand))

	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, root, "commit", "-m", "carry local settings")
	testutil.RunGit(t, root, "rm", "--cached", EntireSettingsLocalFile)

	assert.Empty(t, loadedOPF(t, project, local).Command,
		"content still reachable from HEAD must not be trusted")
}

// With no repository at all there is nothing to clone from, so a local file
// cannot have arrived with someone else's code: it is definitively this
// developer's own. Dropping it here would break every non-repo invocation.
func TestOPFCommandTrust_OutsideGitRepoHonorsLocal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	project := filepath.Join(dir, EntireSettingsFile)
	local := filepath.Join(dir, EntireSettingsLocalFile)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local, localOPFSettings(trustedCommand))

	assert.Equal(t, trustedCommand, loadedOPF(t, project, local).Command,
		"absence of a repository is proof of locality, not a failure to verify")
}

// A .git that exists but cannot be opened is a genuine failure to verify. The
// two policies diverge here: the layer is KEPT (losing every local setting
// over an unreadable repo is worse than the risk), but the exec-bearing
// command is DROPPED (being wrong there means running someone else's binary).
func TestOPFCommandTrust_UnverifiableRepoKeepsLayerDropsCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".entire"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".git"), 0o755)) // present but not a repo
	project := filepath.Join(dir, EntireSettingsFile)
	local := filepath.Join(dir, EntireSettingsLocalFile)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local,
		`{"enabled":true,"redaction":{"openai_privacy_filter":{"command":"`+attackerCommand+`"}},`+
			`"commit_linking":"always"}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	assert.Empty(t, s.Redaction.OpenAIPrivacyFilter.Command,
		"an unverifiable repo must not yield an executed command")
	assert.Equal(t, "always", s.CommitLinking,
		"unrelated local settings must survive an unverifiable repo")
	assert.Empty(t, s.LocalLayerRejection(), "the layer itself was kept")
}

// The gate must scope to `command` only; the rest of the OPF block is
// ordinary configuration and must still merge from the project file.
func TestOPFCommandTrust_OtherOPFFieldsUnaffected(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(attackerCommand))

	s, err := loadMergedSettings(context.Background(), project, "", local)
	require.NoError(t, err)
	opf := s.Redaction.OpenAIPrivacyFilter
	assert.True(t, opf.Enabled, "enabled must still load")
	assert.Equal(t, OPFPromptAlways, opf.PromptDefault, "prompt_default must still load")
	assert.True(t, opf.Categories["private_person"], "categories must still load")
	assert.Empty(t, opf.Command, "only command is gated")
}

func TestLocalSetsOPFCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"command present", `{"redaction":{"openai_privacy_filter":{"command":"x"}}}`, true},
		{"empty command still counts as set", `{"redaction":{"openai_privacy_filter":{"command":""}}}`, true},
		{"other opf fields only", `{"redaction":{"openai_privacy_filter":{"enabled":true}}}`, false},
		{"no opf block", `{"redaction":{"pii":{"enabled":true}}}`, false},
		{"no redaction block", `{"enabled":true}`, false},
		{"malformed json", `{`, false},
		{"redaction not an object", `{"redaction":"nope"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, localSetsOPFCommand([]byte(tc.data)))
		})
	}
}

// pathIsVersioned is memoized for the process lifetime because settings.Load
// runs several times per hook and each probe is a git subprocess. The cached
// answer is deliberately stale within a process; this pins that contract so a
// future change to the cache key is a visible decision.
func TestPathIsVersioned_MemoizesWithinProcess(t *testing.T) {
	t.Parallel()
	root, _, local := newOPFRepo(t)
	writeSettingsFile(t, local, localOPFSettings(attackerCommand))

	first, err := localSettingsIsVersioned(t.Context(), local, true)
	require.NoError(t, err)
	require.False(t, first, "file is untracked to begin with")

	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)

	second, err := localSettingsIsVersioned(t.Context(), local, true)
	require.NoError(t, err)
	assert.False(t, second, "result is cached for the process lifetime")

	ClearVersionedPathCache()
	third, err := localSettingsIsVersioned(t.Context(), local, true)
	require.NoError(t, err)
	assert.True(t, third, "the reset seam must drop the cached verdict")
}

// The probe uses go-git, not the git CLI. That is load-bearing: hooks launched
// by GUI git clients do not inherit a shell profile, and that population is
// the one most likely to need an explicit command. Shelling out would fail
// verification for them and silently drop a legitimate setting.
// Not parallel: t.Setenv.
func TestPathIsVersioned_NoGitBinaryRequired(t *testing.T) {
	root, _, local := newOPFRepo(t)
	writeSettingsFile(t, local, localOPFSettings(trustedCommand))
	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, root, "commit", "-m", "carry local settings")

	t.Setenv("PATH", "")

	versioned, err := probeLocalSettingsIsVersioned(t.Context(), local, true)
	require.NoError(t, err, "probe must not depend on the git binary")
	assert.True(t, versioned, "committed file must still be detected")
}

// A linked worktree keeps its own index under .git/worktrees/<name>/index.
// Reading the main worktree's index instead would answer for the wrong tree.
func TestPathIsVersioned_LinkedWorktreeUsesOwnIndex(t *testing.T) {
	t.Parallel()
	main := t.TempDir()
	testutil.InitRepo(t, main)
	require.NoError(t, os.WriteFile(filepath.Join(main, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, main, "add", "f.txt")
	testutil.RunGit(t, main, "commit", "-m", "init")

	wt := filepath.Join(t.TempDir(), "linked")
	testutil.RunGit(t, main, "worktree", "add", "-b", "feature", wt)
	require.NoError(t, os.MkdirAll(filepath.Join(wt, ".entire"), 0o755))
	local := filepath.Join(wt, EntireSettingsLocalFile)
	require.NoError(t, os.WriteFile(local, []byte(localOPFSettings(trustedCommand)), 0o644))

	versioned, err := probeLocalSettingsIsVersioned(t.Context(), local, true)
	require.NoError(t, err)
	assert.False(t, versioned, "untracked in the linked worktree")

	testutil.RunGit(t, wt, "add", "-f", EntireSettingsLocalFile)
	versioned, err = probeLocalSettingsIsVersioned(t.Context(), local, true)
	require.NoError(t, err)
	assert.True(t, versioned, "must read the linked worktree's own index")
}

// A bare go-git open fails on reftable repositories; gitrepo routes them
// through the reftable storer. Pin that the HEAD lookup survives it.
func TestPathIsVersioned_ReftableRepo(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	init := exec.CommandContext(t.Context(), "git", "init", "-q", "--ref-format=reftable", ".")
	init.Dir = root
	init.Env = testutil.GitIsolatedEnv()
	out, err := init.CombinedOutput()
	require.NoError(t, err, "reftable init: %s", out)
	for _, kv := range [][]string{{"user.email", "t@t.io"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
		testutil.RunGit(t, root, "config", kv[0], kv[1])
	}
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".entire"), 0o755))
	local := filepath.Join(root, EntireSettingsLocalFile)
	require.NoError(t, os.WriteFile(local, []byte(localOPFSettings(trustedCommand)), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644))
	testutil.RunGit(t, root, "add", "f.txt")
	testutil.RunGit(t, root, "commit", "-m", "init")

	// Committed then unstaged: answerable only from HEAD, which is the part
	// that needs the reftable storer.
	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, root, "commit", "-m", "carry")
	testutil.RunGit(t, root, "rm", "--cached", EntireSettingsLocalFile)

	versioned, err := probeLocalSettingsIsVersioned(t.Context(), local, true)
	require.NoError(t, err, "reftable repo must be verifiable")
	assert.True(t, versioned, "HEAD lookup must work on reftable")
}

// The whole local layer — not just the executed command — is ignored when the
// file is tracked. A committed settings.local.json is not local: it arrives by
// cloning and silently overrides project settings for everyone.
func TestLocalLayer_TrackedFileIsIgnoredEntirely(t *testing.T) {
	t.Parallel()
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true,"commit_linking":"prompt"}`)
	writeSettingsFile(t, local, `{"commit_linking":"always","external_agents":true}`)
	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	assert.Equal(t, "prompt", s.CommitLinking, "tracked local override must not apply")
	assert.False(t, s.ExternalAgents, "tracked local override must not apply")
	assert.Contains(t, s.LocalLayerRejection(), "tracked in git",
		"the rejection must be reportable to the user")
}

// The supported case still works: an untracked local file overrides normally.
func TestLocalLayer_UntrackedFileApplies(t *testing.T) {
	t.Parallel()
	_, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, `{"enabled":true,"commit_linking":"prompt"}`)
	writeSettingsFile(t, local, `{"commit_linking":"always"}`)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	assert.Equal(t, "always", s.CommitLinking, "untracked local override applies")
	assert.Empty(t, s.LocalLayerRejection())
}

// newOPFRepo leaves HEAD unborn (testutil.InitRepo makes no commit), so every
// test above also exercises the unborn-HEAD branch of the deep probe. This is
// the other branch: a repository with history, local file untracked.
func TestOPFCommandTrust_BornHeadHonorsUntrackedLocal(t *testing.T) {
	t.Parallel()
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	testutil.RunGit(t, root, "add", EntireSettingsFile)
	testutil.RunGit(t, root, "commit", "-m", "project settings")
	writeSettingsFile(t, local, localOPFSettings(trustedCommand))

	assert.Equal(t, trustedCommand, loadedOPF(t, project, local).Command,
		"an untracked local override is honored in a repo with history too")
}

// The layer check is index-only; the command check also consults HEAD. This
// pins the one state where they diverge: committed, then `git rm --cached`.
// Checkout cannot produce a file absent from the index, so that state is one
// the local developer created — safe for preferences, not for an exec.
func TestLocalLayer_CommittedThenUnstagedKeepsLayerDropsCommand(t *testing.T) {
	t.Parallel()
	root, project, local := newOPFRepo(t)
	writeSettingsFile(t, project, opfSettings(""))
	writeSettingsFile(t, local,
		`{"redaction":{"openai_privacy_filter":{"command":"`+attackerCommand+`"}},`+
			`"commit_linking":"always"}`)
	testutil.RunGit(t, root, "add", "-f", EntireSettingsLocalFile)
	testutil.RunGit(t, root, "commit", "-m", "carry local settings")
	testutil.RunGit(t, root, "rm", "--cached", EntireSettingsLocalFile)

	s, err := loadMergedSettings(t.Context(), project, "", local)
	require.NoError(t, err)

	assert.Equal(t, "always", s.CommitLinking, "index-only: the layer still applies")
	assert.Empty(t, s.LocalLayerRejection(), "the layer was not rejected")
	assert.Empty(t, s.Redaction.OpenAIPrivacyFilter.Command,
		"deep check: HEAD presence still disqualifies the executed command")
}
