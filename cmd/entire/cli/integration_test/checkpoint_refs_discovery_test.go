//go:build integration

package integration

import (
	"strings"
	"testing"
)

// TestCheckpointRefsDiscovery_CrossMachine: git-refs only — a fresh clone
// with zero local checkpoint state discovers refs-native checkpoints on its
// remote WITHOUT a dedicated checkpoint_remote, then hydrates them on read.
// This closes the default-setup gap scoped out in #1771: discovery previously
// required a configured checkpoint_remote, so `checkpoint list` and the
// branch explain view showed nothing on a second machine.
func TestCheckpointRefsDiscovery_CrossMachine(t *testing.T) {
	t.Parallel()

	env := NewFeatureBranchEnv(t)
	env.CheckpointStore = StoreGitRefs

	bareOrigin := env.SetupBareRemote()

	checkpointID := createCheckpointedCommit(t, env, "Add machine module", "machine.go", "package machine", "Add machine module")
	env.GitPush("origin", "HEAD")
	env.RunPrePush("origin")
	if !env.CheckpointExistsOnRemote(bareOrigin, checkpointID) {
		t.Fatalf("checkpoint %s should be on origin", checkpointID)
	}

	// Machine B: a fresh clone with no local checkpoint refs and no dedicated
	// checkpoint_remote. The list assertion is discovery-load-bearing: the
	// branch view only surfaces a checkpoint whose ID store.List knows, and a
	// pristine clone has no local refs, so only remote discovery can supply
	// it (known-ID hydration fires on explain, asserted separately).
	cloneEnv := env.CloneFrom(bareOrigin)
	if cloneEnv.CheckpointsPresentLocally() {
		t.Fatal("fixture: the clone must start with zero local checkpoint state")
	}

	listOutput := cloneEnv.RunCLI("checkpoint", "list")
	if !strings.Contains(listOutput, "Add machine module") && !strings.Contains(listOutput, checkpointID[:8]) {
		t.Errorf("cross-machine discovery should surface the remote checkpoint, got:\n%s", listOutput)
	}

	output := cloneEnv.RunCLI("checkpoint", "explain", "--checkpoint", checkpointID)
	if !strings.Contains(output, "Add machine module") {
		t.Errorf("the discovered checkpoint should hydrate on read, got:\n%s", output)
	}
}
