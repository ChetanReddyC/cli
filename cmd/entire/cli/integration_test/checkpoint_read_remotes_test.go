//go:build integration

package integration

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Checkpoint read candidates (ENCLI-255 / #1634)
//
// Checkpoint WRITES sync to one elected remote (see
// checkpoint_sync_remote_test.go); READS consult an ordered candidate chain —
// the elected remote first, then "origin" as a read-only legacy tier. These
// end-to-end tests drive real reads (`checkpoint explain --checkpoint`, which
// fetches on miss through the resume metadata chain / RefFetcher, and
// `checkpoint list`, which triggers git-refs remote discovery) against
// multi-remote topologies. The per-site candidate semantics are unit-tested
// next to each rewired function.
// =============================================================================

// wipeLocalCheckpointState removes every local checkpoint read source — the
// v1 branch, both remotes' v1 tracking refs, and (git-refs) every local
// per-checkpoint ref — so a subsequent read can only succeed via a remote.
func wipeLocalCheckpointState(t *testing.T, env *TestEnv) {
	t.Helper()
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)

	for _, ref := range []plumbing.ReferenceName{
		plumbing.NewBranchReferenceName(paths.MetadataBranchName),
		plumbing.NewRemoteReferenceName("origin", paths.MetadataBranchName),
		plumbing.NewRemoteReferenceName("upstream", paths.MetadataBranchName),
	} {
		_ = repo.Storer.RemoveReference(ref) //nolint:errcheck // refs may legitimately not exist per scenario
	}

	if env.usingGitRefs() {
		iter, err := repo.References()
		require.NoError(t, err)
		var toDelete []plumbing.ReferenceName
		require.NoError(t, iter.ForEach(func(r *plumbing.Reference) error {
			if strings.HasPrefix(r.Name().String(), checkpointRefPrefix) {
				toDelete = append(toDelete, r.Name())
			}
			return nil
		}))
		for _, name := range toDelete {
			require.NoError(t, repo.Storer.RemoveReference(name))
		}
	}
}

// localPrimaryExists reports whether refs/heads/entire/checkpoints/v1 exists
// in the test repo.
func localPrimaryExists(t *testing.T, env *TestEnv) bool {
	t.Helper()
	repo, err := git.PlainOpen(env.RepoDir)
	require.NoError(t, err)
	_, err = repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	return err == nil
}

// A parentless commit built from the worktree tree has no checkpoint metadata,
// which distinguishes branch existence from the requested checkpoint existing.
func rootlessWorktreeCommit(t *testing.T, env *TestEnv) string {
	t.Helper()
	return env.gitOutput(env.RepoDir, "commit-tree", "HEAD^{tree}", "-m", "checkpoint root")
}

// TestCheckpointReadRemotes_ConfiguredReadBack: the election/read round trip
// for a configured user. checkpoint_push_remote elects "upstream", the
// checkpoint is pushed there through the real gate, origin exists and stays
// empty — then, with all local checkpoint state wiped, an explain-style read
// finds the checkpoint on the elected remote.
func TestCheckpointReadRemotes_ConfiguredReadBack(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		bareUpstream := env.SetupNamedBareRemote("upstream")
		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "upstream",
			},
		})

		checkpointID := createCheckpointedCommit(t, env, "Add readback module", "readback.go", "package readback", "Add readback module")
		env.RunPrePush("upstream")
		if !env.CheckpointExistsOnRemote(bareUpstream, checkpointID) {
			t.Fatalf("checkpoint %s should be on the elected remote", checkpointID)
		}
		if env.CheckpointsPresentOnRemote(bareOrigin) {
			t.Fatal("origin must stay empty in this scenario")
		}

		wipeLocalCheckpointState(t, env)

		output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		if !strings.Contains(output, "Add readback module") {
			t.Errorf("read must find the checkpoint on the elected remote, got:\n%s", output)
		}

		if env.usingGitRefs() {
			// Discovery (checkpoint list) finds the elected remote's refs
			// without a dedicated checkpoint_remote.
			wipeLocalCheckpointState(t, env)
			listOutput := env.RunCLI("checkpoint", "list")
			if !strings.Contains(listOutput, "Add readback module") && !strings.Contains(listOutput, checkpointID[:8]) {
				t.Errorf("git-refs discovery should surface the elected remote's checkpoint, got:\n%s", listOutput)
			}
		}
	})
}

// TestCheckpointReadRemotes_LegacyOriginTierServed: pre-election data lives
// on origin only; the election then moves to "upstream". Reads must fall back
// to the legacy origin tier and find the data — and, on git-branch, must do
// so WITHOUT recreating the local primary from the legacy tier (the read-only
// rule; the data is served through origin's tracking ref).
func TestCheckpointReadRemotes_LegacyOriginTierServed(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		_ = env.SetupNamedBareRemote("upstream")

		// Checkpoint lands on origin under the default election.
		checkpointID := createCheckpointedCommit(t, env, "Add legacy module", "legacy.go", "package legacy", "Add legacy module")
		env.RunPrePush("origin")
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Fatalf("checkpoint %s should be on origin", checkpointID)
		}

		// The election moves to upstream AFTER the data landed on origin —
		// the fork-adoption shape that makes origin the legacy tier.
		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "upstream",
			},
		})

		wipeLocalCheckpointState(t, env)
		localPrimaryHash := ""
		if !env.usingGitRefs() {
			localPrimaryHash = rootlessWorktreeCommit(t, env)
			env.gitOutput(env.RepoDir, "update-ref", "refs/heads/"+paths.MetadataBranchName, localPrimaryHash)
		}

		output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		if !strings.Contains(output, "Add legacy module") {
			t.Errorf("read must fall back to the legacy origin tier, got:\n%s", output)
		}

		if !env.usingGitRefs() {
			got := env.refHash(env.RepoDir, "refs/heads/"+paths.MetadataBranchName)
			if got != localPrimaryHash {
				t.Errorf("legacy reads must not rewrite the local primary: got %s, want %s", got, localPrimaryHash)
			}
		}

		if env.usingGitRefs() {
			// Discovery unions origin's legacy refs in even though the
			// election points at upstream.
			wipeLocalCheckpointState(t, env)
			listOutput := env.RunCLI("checkpoint", "list")
			if !strings.Contains(listOutput, "Add legacy module") && !strings.Contains(listOutput, checkpointID[:8]) {
				t.Errorf("git-refs discovery should union the legacy origin tier's refs, got:\n%s", listOutput)
			}
		}
	})
}

// An elected metadata branch may exist without containing a checkpoint that
// still lives on the legacy origin branch. Selection must happen per requested
// checkpoint, not merely per branch existence.
func TestCheckpointReadRemotes_ElectedBranchMissingCheckpointFallsBackToOrigin(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitBranch

	bareOrigin := env.SetupBareRemote()
	_ = env.SetupNamedBareRemote("upstream")

	checkpointID := createCheckpointedCommit(t, env, "Add split module", "split.go", "package split", "Add split module")
	env.RunPrePush("origin")
	if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
		t.Fatalf("checkpoint %s should be on origin", checkpointID)
	}

	upstreamHash := rootlessWorktreeCommit(t, env)
	env.gitOutput(env.RepoDir, "push", "--quiet", "--no-verify", "upstream", upstreamHash+":refs/heads/"+paths.MetadataBranchName)
	env.PatchSettings(map[string]any{
		"strategy_options": map[string]any{
			"checkpoint_push_remote": "upstream",
		},
	})

	wipeLocalCheckpointState(t, env)

	output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
	if !strings.Contains(output, "Add split module") {
		t.Errorf("read must continue to origin when the elected branch lacks the checkpoint, got:\n%s", output)
	}
}

// TestCheckpointReadRemotes_UpstreamOnlyRepo: a repo with no origin at all —
// the sole remote wins the election and reads work against it.
func TestCheckpointReadRemotes_UpstreamOnlyRepo(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareUpstream := env.SetupNamedBareRemote("upstream")

		checkpointID := createCheckpointedCommit(t, env, "Add solo module", "solo.go", "package solo", "Add solo module")
		env.RunPrePush("upstream")
		if !env.CheckpointExistsOnRemote(bareUpstream, checkpointID) {
			t.Fatalf("checkpoint %s should be on the sole remote", checkpointID)
		}

		wipeLocalCheckpointState(t, env)

		output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		if !strings.Contains(output, "Add solo module") {
			t.Errorf("reads must work against the sole (non-origin) remote, got:\n%s", output)
		}
	})
}

// TestCheckpointReadRemotes_RemotelessRepoClassifiesAbsent: a fully local
// repository keeps its "checkpoint not found" classification — a read miss is
// absence, never a transport outage.
func TestCheckpointReadRemotes_RemotelessRepoClassifiesAbsent(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		checkpointID := createCheckpointedCommit(t, env, "Add hermit module", "hermit.go", "package hermit", "Add hermit module")
		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}

		wipeLocalCheckpointState(t, env)

		output, err := env.RunCLIWithError("checkpoint", "explain", "--checkpoint", checkpointID)
		if err == nil {
			t.Fatalf("explain should fail for a wiped checkpoint in a remoteless repo, got:\n%s", output)
		}
		if !strings.Contains(output, "not found") {
			t.Errorf("a remoteless miss must classify as not-found (absence), got:\n%s", output)
		}
	})
}

// TestCheckpointReadRemotes_ElectedUnreachableLegacyStillServes: the
// end-to-end local-ref confinement pin. The elected remote (upstream) becomes
// unreachable after the legacy data landed on origin; reads still succeed via
// the legacy tier while the local primary stays untouched (git-branch — the
// v1 branch is the local ref the #1374 hazard concerns).
func TestCheckpointReadRemotes_ElectedUnreachableLegacyStillServes(t *testing.T) {
	t.Parallel()
	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareOrigin := env.SetupBareRemote()
		_ = env.SetupNamedBareRemote("upstream")

		checkpointID := createCheckpointedCommit(t, env, "Add outage module", "outage.go", "package outage", "Add outage module")
		env.RunPrePush("origin")
		if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
			t.Fatalf("checkpoint %s should be on origin", checkpointID)
		}

		env.PatchSettings(map[string]any{
			"strategy_options": map[string]any{
				"checkpoint_push_remote": "upstream",
			},
		})
		// The elected remote goes dark: point it at a path that doesn't exist.
		cmd := exec.CommandContext(t.Context(), "git", "remote", "set-url", "upstream", env.RepoDir+"/nonexistent-remote")
		cmd.Dir = env.RepoDir
		cmd.Env = testutil.GitIsolatedEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git remote set-url: %v\n%s", err, out)
		}
		env.setGitConfigBaseline()

		wipeLocalCheckpointState(t, env)

		output := env.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
		if !strings.Contains(output, "Add outage module") {
			t.Errorf("reads must survive an unreachable elected remote via the legacy tier, got:\n%s", output)
		}
		if !env.usingGitRefs() && localPrimaryExists(t, env) {
			t.Error("a stale/legacy origin must never recreate the local primary, even with the elected remote unreachable")
		}
	})
}
