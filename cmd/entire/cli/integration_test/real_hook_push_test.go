//go:build integration

package integration

import (
	"os/exec"
	"testing"
)

// TestGitPushWithHooks_SyncsCheckpointsToRemote is the seed of test A1: a plain
// `git push` of a feature branch, running the installed pre-push hook exactly as
// git runs it (realistic stdin refspecs, remote name/URL argv), lands the
// committed checkpoints on the bare remote WITHOUT any explicit RunPrePush or
// PushCheckpointRefs. It runs under both checkpoint backends via ForEachBackend,
// validating the whole I-1/I-2 enabler stack: env injection selects the store,
// the real hook drains it, and the backend-aware assertion finds the result.
func TestGitPushWithHooks_SyncsCheckpointsToRemote(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareDir := env.SetupBareRemote()

		checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}

		// Sanity: checkpoint exists locally under the selected backend.
		if !env.CheckpointsPresentLocally() {
			t.Fatalf("[%s] checkpoint should exist locally after condensation", backend)
		}

		// Plain push through the real hook — no explicit checkpoint push.
		env.GitPushWithHooks("origin", "HEAD")

		if !env.CheckpointsPresentOnRemote(bareDir) {
			t.Fatalf("[%s] checkpoints should be on remote after `git push` via the real pre-push hook", backend)
		}
		if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
			t.Fatalf("[%s] checkpoint %s should be on remote after `git push` via the real pre-push hook", backend, checkpointID)
		}
	})
}

// TestGitPushWithHooks_DefersCheckpointsUntilFirstUserBranchExists ensures a
// repository's first remote branch is the user's branch, not Entire metadata.
// On a hosting provider that assigns the first branch as default, publishing
// checkpoints in this hook invocation would expose entire/checkpoints/v1 as
// the repository default branch.
func TestGitPushWithHooks_DefersCheckpointsUntilFirstUserBranchExists(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		bareDir := env.SetupEmptyNamedBareRemote("origin")
		branch := env.GetCurrentBranch()
		checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
		if checkpointID == "" {
			t.Fatal("should have a checkpoint ID after condensation")
		}

		// The first normal branch push must complete, but must not publish
		// checkpoint metadata to an otherwise empty remote.
		env.GitPushWithHooks("origin", "HEAD")
		if !env.BranchExistsOnRemote(bareDir, branch) {
			t.Fatalf("[%s] first user branch %q should be on remote", backend, branch)
		}
		if env.CheckpointsPresentOnRemote(bareDir) {
			t.Fatalf("[%s] checkpoints must be deferred until after the first user branch push", backend)
		}

		// A later normal push may publish the deferred checkpoint metadata.
		env.WriteFile("later.go", "package later")
		env.GitAdd("later.go")
		env.GitCommit("Later user commit")
		env.GitPushWithHooks("origin", "HEAD")
		if !env.CheckpointExistsOnRemote(bareDir, checkpointID) {
			t.Fatalf("[%s] deferred checkpoint %s should be published on a later push", backend, checkpointID)
		}
	})
}

func TestGitPushWithHooks_DefersCheckpointsUntilPushURLHasUserBranch(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		// origin's fetch URL already has the user branch, but pushes are routed
		// to a different empty repository. The guard must inspect the latter.
		_ = env.SetupBareRemote()
		pushTarget := env.SetupEmptyNamedBareRemote("push-target")
		cmd := exec.CommandContext(t.Context(), "git", "remote", "set-url", "--push", "origin", pushTarget)
		cmd.Dir = env.RepoDir
		cmd.Env = env.cliEnv()
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("set origin pushurl: %v\n%s", err, output)
		}
		env.setGitConfigBaseline()

		checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")
		env.GitPushWithHooks("origin", "HEAD")
		if env.CheckpointsPresentOnRemote(pushTarget) {
			t.Fatalf("[%s] checkpoints must be deferred on the empty pushurl target", backend)
		}

		env.WriteFile("later.go", "package later")
		env.GitAdd("later.go")
		env.GitCommit("Later user commit")
		env.GitPushWithHooks("origin", "HEAD")
		if !env.CheckpointExistsOnRemote(pushTarget, checkpointID) {
			t.Fatalf("[%s] deferred checkpoint %s should be published to pushurl on a later push", backend, checkpointID)
		}
	})
}

// TestGitPushWithHooks_DefersCheckpointsWhenAnyPushURLTargetIsEmpty covers a
// remote configured with multiple push destinations. `git push` writes to all
// of them, so as long as any one destination is still empty, publishing
// checkpoints could make entire/checkpoints/v1 its first (default) branch. The
// guard must defer until every push target has a branch.
func TestGitPushWithHooks_DefersCheckpointsWhenAnyPushURLTargetIsEmpty(t *testing.T) {
	t.Parallel()

	ForEachBackend(t, func(t *testing.T, backend string) {
		env := NewFeatureBranchEnv(t)
		env.CheckpointStore = backend

		// origin fetches from its own bare, but pushes fan out to two targets:
		// one that already has the user branch, and one that is still empty.
		_ = env.SetupBareRemote()
		populated := env.SetupNamedBareRemote("populated")
		empty := env.SetupEmptyNamedBareRemote("empty")
		for i, url := range []string{populated, empty} {
			args := []string{"remote", "set-url", "--push", "origin", url}
			if i > 0 {
				args = []string{"remote", "set-url", "--push", "--add", "origin", url}
			}
			cmd := exec.CommandContext(t.Context(), "git", args...)
			cmd.Dir = env.RepoDir
			cmd.Env = env.cliEnv()
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("set origin pushurl %q: %v\n%s", url, err, output)
			}
		}
		env.setGitConfigBaseline()

		checkpointID := createCheckpointedCommit(t, env, "Add auth module", "auth.go", "package auth", "Add auth module")

		// The empty push target must force deferral even though the other target
		// already has a branch.
		env.GitPushWithHooks("origin", "HEAD")
		if env.CheckpointsPresentOnRemote(empty) {
			t.Fatalf("[%s] checkpoints must be deferred while a push target is empty", backend)
		}
		if env.CheckpointsPresentOnRemote(populated) {
			t.Fatalf("[%s] checkpoints must be deferred on every target while any push target is empty", backend)
		}

		// The first push gave the previously-empty target the user branch, so a
		// later push may publish to both.
		env.WriteFile("later.go", "package later")
		env.GitAdd("later.go")
		env.GitCommit("Later user commit")
		env.GitPushWithHooks("origin", "HEAD")
		if !env.CheckpointExistsOnRemote(empty, checkpointID) {
			t.Fatalf("[%s] deferred checkpoint %s should be published once every target has a branch", backend, checkpointID)
		}
	})
}
