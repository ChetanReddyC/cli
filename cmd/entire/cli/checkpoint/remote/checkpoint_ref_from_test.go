package remote

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// candidatesFixture creates a work repo with two remotes — "upstream" (the
// elected/first read candidate) and "origin" (the legacy tier) — each backed
// by a local bare repo, and chdirs into the work repo. The checkpoint ref is
// pushed to each remote per the flags, pointing at DIFFERENT commits so a
// test can prove which candidate served a fetch.
func candidatesFixture(t *testing.T, refOnUpstream, refOnOrigin bool) (workDir string, ref plumbing.ReferenceName, upstreamHash, originHash string) {
	t.Helper()
	testutil.IsolateGitConfigEnv(t)

	upstreamBare := t.TempDir()
	originBare := t.TempDir()
	for _, bare := range []string{upstreamBare, originBare} {
		out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", bare).CombinedOutput()
		require.NoError(t, err, "git init --bare: %s", out)
	}

	workDir = t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "one")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "one")
	firstCommit := candidateRevParse(t, workDir, "HEAD")
	testutil.WriteFile(t, workDir, "g.txt", "two")
	testutil.GitAdd(t, workDir, "g.txt")
	testutil.GitCommit(t, workDir, "two")
	secondCommit := candidateRevParse(t, workDir, "HEAD")

	testutil.AddRemote(t, workDir, "upstream", upstreamBare)
	testutil.AddRemote(t, workDir, "origin", originBare)

	ref = plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	if refOnUpstream {
		upstreamHash = secondCommit
		pushRefTo(t, workDir, "upstream", secondCommit, ref)
	}
	if refOnOrigin {
		originHash = firstCommit
		pushRefTo(t, workDir, "origin", firstCommit, ref)
	}

	t.Chdir(workDir)
	return workDir, ref, upstreamHash, originHash
}

func pushRefTo(t *testing.T, workDir, remoteName, hash string, ref plumbing.ReferenceName) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "push", "--quiet", remoteName, hash+":"+ref.String()).CombinedOutput()
	require.NoError(t, err, "git push checkpoint ref: %s", out)
}

func candidateRevParse(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", rev).Output()
	require.NoError(t, err, "git rev-parse %s", rev)
	return strings.TrimSpace(string(out))
}

func localRefHash(t *testing.T, dir string, ref plumbing.ReferenceName) string {
	t.Helper()
	return candidateRevParse(t, dir, ref.String())
}

// TestFetchCheckpointRefFrom_FirstCandidateWins: the ref exists on both
// candidates at different commits; the fetch must serve it from the FIRST
// candidate (the elected remote), proven by the resulting local ref hash.
func TestFetchCheckpointRefFrom_FirstCandidateWins(t *testing.T) {
	workDir, ref, upstreamHash, originHash := candidatesFixture(t, true, true)

	require.NoError(t, FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}))

	got := localRefHash(t, workDir, ref)
	require.Equal(t, upstreamHash, got, "the first candidate must serve the fetch")
	require.NotEqual(t, originHash, got)
}

// TestFetchCheckpointRefFrom_LegacyTierServesRefMissingOnElected: the elected
// candidate lacks the ref (pre-election legacy data lives on origin); the
// fetch must advance to the legacy origin tier and succeed.
func TestFetchCheckpointRefFrom_LegacyTierServesRefMissingOnElected(t *testing.T) {
	workDir, ref, _, originHash := candidatesFixture(t, false, true)

	require.NoError(t, FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}))

	require.Equal(t, originHash, localRefHash(t, workDir, ref))
}

// TestFetchCheckpointRefFrom_TransportErrorAdvances: a transport-level
// failure on the first candidate advances to the next candidate.
func TestFetchCheckpointRefFrom_TransportErrorAdvances(t *testing.T) {
	workDir, ref, _, originHash := candidatesFixture(t, false, true)
	out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "set-url", "upstream", workDir+"/nonexistent-remote").CombinedOutput()
	require.NoError(t, err, "%s", out)

	require.NoError(t, FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"}))

	require.Equal(t, originHash, localRefHash(t, workDir, ref))
}

// TestFetchCheckpointRefFrom_AllFailSurfacesFirstCandidateError: when every
// candidate fails, the FIRST candidate's error is surfaced — here a transport
// failure on the elected remote, which must NOT be masked by the second
// candidate's "ref not found" (that would misclassify a possibly-existing
// checkpoint as absent).
func TestFetchCheckpointRefFrom_AllFailSurfacesFirstCandidateError(t *testing.T) {
	workDir, ref, _, _ := candidatesFixture(t, false, false)
	out, err := exec.CommandContext(t.Context(), "git", "-C", workDir, "remote", "set-url", "upstream", workDir+"/nonexistent-remote").CombinedOutput()
	require.NoError(t, err, "%s", out)

	err = FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"})
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"the first candidate's transport error must be surfaced, not the legacy tier's absence")
	require.Contains(t, err.Error(), "probe checkpoint ref")
}

// TestFetchCheckpointRefFrom_AbsentOnEveryCandidateIsAbsence: every candidate
// is reachable and none has the ref — genuine absence, wrapping
// plumbing.ErrReferenceNotFound (the first candidate's error).
func TestFetchCheckpointRefFrom_AbsentOnEveryCandidateIsAbsence(t *testing.T) {
	_, ref, _, _ := candidatesFixture(t, false, false)

	err := FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"})
	require.Error(t, err)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
}

// TestFetchCheckpointRefFrom_EmptyChainNoRemotesIsAbsence: the four-condition
// absence classification — empty candidate chain, emptiness proven by a
// successful empty `git remote` listing, live context, readable settings with
// no checkpoint_remote key — classifies the ref as absent.
func TestFetchCheckpointRefFrom_EmptyChainNoRemotesIsAbsence(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	t.Chdir(workDir)

	ref := plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	err := FetchCheckpointRefFrom(context.Background(), ref, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a provably remoteless repo must classify the ref as absent")
}

// TestFetchCheckpointRefFrom_EmptyChainWithRemotesIsNotAbsence: an empty
// chain whose emptiness is NOT backed by a remoteless repo (a fail-closed
// election left a configured remote out of the chain) must surface an error,
// never absence.
func TestFetchCheckpointRefFrom_EmptyChainWithRemotesIsNotAbsence(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	bareDir := t.TempDir()
	out, err := exec.CommandContext(t.Context(), "git", "init", "--bare", bareDir).CombinedOutput()
	require.NoError(t, err, "git init --bare: %s", out)

	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	testutil.AddRemote(t, workDir, "upstream", bareDir)
	t.Chdir(workDir)

	ref := plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	err = FetchCheckpointRefFrom(context.Background(), ref, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a repo with configured remotes must never classify an empty chain as absence")
}

// TestFetchCheckpointRefFrom_CanceledContextIsNotAbsence: a dead caller
// context invalidates the remote listing as evidence — the empty chain must
// surface an error, never absence.
func TestFetchCheckpointRefFrom_CanceledContextIsNotAbsence(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	t.Chdir(workDir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ref := plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	err := FetchCheckpointRefFrom(ctx, ref, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a canceled context must stay a failure, never absence")
}

// TestFetchCheckpointRefFrom_CheckpointRemoteKeyIsNotAbsence: a
// checkpoint_remote key present in any form (here malformed) rules out the
// remoteless classification even with an empty chain.
func TestFetchCheckpointRefFrom_CheckpointRemoteKeyIsNotAbsence(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	testutil.WriteFile(t, workDir, ".entire/settings.json",
		`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "github"}}}`)
	t.Chdir(workDir)

	ref := plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	err := FetchCheckpointRefFrom(context.Background(), ref, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a present-but-malformed checkpoint_remote must not classify as absence")
}

// TestFetchCheckpointRefFrom_UnreadableSettingsIsNotAbsence: unreadable
// settings cannot rule out a configured checkpoint remote — the call falls
// back to the legacy single-target behavior and surfaces a failure.
func TestFetchCheckpointRefFrom_UnreadableSettingsIsNotAbsence(t *testing.T) {
	testutil.IsolateGitConfigEnv(t)
	workDir := t.TempDir()
	testutil.InitRepo(t, workDir)
	testutil.WriteFile(t, workDir, "f.txt", "content")
	testutil.GitAdd(t, workDir, "f.txt")
	testutil.GitCommit(t, workDir, "init")
	testutil.WriteFile(t, workDir, ".entire/settings.json", "{not valid json")
	t.Chdir(workDir)

	ref := plumbing.ReferenceName("refs/entire/checkpoints/Z9/01KVBJCWYA4YW6J5M9GP655HZ9")
	err := FetchCheckpointRefFrom(context.Background(), ref, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"unreadable settings must not classify as absence")
}

// TestFetchCheckpointRefFrom_DedicatedCheckpointRemoteBypassesCandidates: a
// configured checkpoint_remote is a dedicated store — the candidate chain
// does not apply and the legacy single-target semantics hold. The ref exists
// ONLY on the first candidate (upstream): if the chain were consulted the
// fetch would succeed, so the surfaced refusal error proves the bypass.
func TestFetchCheckpointRefFrom_DedicatedCheckpointRemoteBypassesCandidates(t *testing.T) {
	workDir, ref, _, _ := candidatesFixture(t, true, false)
	testutil.WriteFile(t, workDir, ".entire/settings.json",
		`{"enabled": true, "strategy_options": {"checkpoint_remote": {"provider": "bogusforge", "repo": "acme/checkpoints"}}}`)

	err := FetchCheckpointRefFrom(context.Background(), ref, []string{"upstream", "origin"})
	require.Error(t, err)
	require.NotErrorIs(t, err, plumbing.ErrReferenceNotFound,
		"a dedicated checkpoint_remote must keep the single-target semantics")
}
