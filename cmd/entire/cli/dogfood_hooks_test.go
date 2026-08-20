package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

// TestCommittedHookConfigsAreCurrent pins this repo's own committed Entire hook
// configs against what the CLI would install today.
//
// This repo commits its dogfood hook configs so every clone gets checkpointing
// without each person running `entire enable`. Those copies are generated from
// templates that live in the same tree and keep moving, and nothing regenerates
// them — so they rot silently. Neither guard that looks at them catches it:
// AreHooksInstalled only greps the ownership marker, which survives any template
// change, and Pi's fireHook swallows every error by design. The committed Pi
// extension sat two commits behind its template for three weeks that way, which
// cost anyone dogfooding Pi here the ENTIRE_PI_NESTED guard — a subagent
// forwarded its own lifecycle as if it were the user's session.
//
// CheckHookConfig already detects this; it just had no caller on the CI path, so
// only whoever happened to run `entire doctor` would see it. Running it here
// fails the next template edit in CI instead of on a teammate's machine.
//
// When this fails: run `entire enable --force` at the repo root and commit the
// regenerated files.
func TestCommittedHookConfigsAreCurrent(t *testing.T) {
	// t.Chdir is process-global, so this test must not be parallel. It is needed
	// because CheckHookConfig resolves the config paths from the working
	// directory via paths.WorktreeRoot. That resolution is cached, but the cache
	// is keyed on the working directory, so chdir invalidates it rather than
	// handing us a root some earlier test left behind.
	t.Chdir(repoRootFromSource(t))

	ctx := context.Background()
	checked := 0
	for _, name := range agent.List() {
		ag, err := agent.Get(name)
		if err != nil {
			t.Errorf("agent.Get(%s): %v", name, err)
			continue
		}
		hf, ok := agent.AsHookFreshness(ag)
		if !ok {
			continue // this agent has no drift check
		}
		switch hf.CheckHookConfig(ctx) {
		case agent.HooksAbsent:
			// This repo commits no config for this agent — nothing to pin.
		case agent.HooksCurrent:
			checked++
		case agent.HooksOutdated:
			checked++
			t.Errorf("committed %s hook config is stale: it no longer matches what "+
				"InstallHooks writes today. Regenerate with `entire enable --force` "+
				"at the repo root and commit the result.", ag.Type())
		}
	}

	// Without this the test passes vacuously if the committed configs are moved
	// or renamed: every agent would report HooksAbsent and nothing would be
	// compared. Failing loudly is the point — a drift guard that quietly stops
	// guarding is worse than none.
	if checked == 0 {
		t.Fatal("no committed hook configs were checked; expected at least one " +
			"(.pi/extensions/entire/index.ts, .opencode/plugins/entire.ts, " +
			".claude/settings.json). Either they moved or CheckHookConfig now " +
			"reports HooksAbsent for all of them")
	}
}

// repoRootFromSource returns this repo's root, derived from this file's own
// compiled-in path rather than the working directory: package cli has tests that
// t.Chdir, and the root must not depend on what an earlier one left behind.
func repoRootFromSource(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no caller information")
	}
	// This file lives at <root>/cmd/entire/cli/dogfood_hooks_test.go.
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("derived repo root %q has no go.mod (did this file move?): %v", root, err)
	}
	return root
}
