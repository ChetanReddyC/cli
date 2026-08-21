//go:build e2e

package tests

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/e2e/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFactoryTaskRecordExistsBeforeCommit(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		if s.Agent.Name() != "factoryai-droid" {
			t.Skip("factory-only regression test")
		}

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skip("factoryai-droid does not support interactive mode")
		}

		s.WaitFor(t, session, s.Agent.PromptPattern(), 30*time.Second)
		s.Send(t, session,
			"Can you run a Worker that inspects this code and comes up with a short summary about what it is about? Have the Worker write that summary to docs/factory-hook-check.md as one short paragraph followed by exactly 3 bullet points. Do not create or edit the file in the main agent process. Only the Worker should write the file. Do not commit. Do not ask for confirmation.")
		s.WaitFor(t, session, s.Agent.PromptPattern(), 90*time.Second)

		// WaitFor can return while droid is still working: the input box's
		// "> Enter to steer" matches the prompt pattern even mid-turn, so the
		// file wait must absorb the Worker's runtime (60-120s turns on CI).
		testutil.WaitForFileExists(t, s.Dir, "docs/factory-hook-check.md", 120*time.Second)

		waitForCompletedTaskRecord(t, s.Dir, 30*time.Second)
		assertNoShadowTaskData(t, s.Dir)
	})
}

func TestFactoryCommittedCheckpointExcludesPreExistingUntrackedFiles(t *testing.T) {
	testutil.ForEachAgent(t, 3*time.Minute, func(t *testing.T, s *testutil.RepoState, ctx context.Context) {
		if s.Agent.Name() != "factoryai-droid" {
			t.Skip("factory-only regression test")
		}

		sentinelPath := filepath.Join(s.Dir, "docs", "factory-preexisting-human-note.md")
		require.NoError(t, os.MkdirAll(filepath.Dir(sentinelPath), 0o755))
		require.NoError(t, os.WriteFile(sentinelPath, []byte("human-owned sentinel\n"), 0o644))

		session := s.StartSession(t, ctx)
		if session == nil {
			t.Skip("factoryai-droid does not support interactive mode")
		}

		s.WaitFor(t, session, s.Agent.PromptPattern(), 30*time.Second)
		s.Send(t, session,
			"Can you run a Worker that inspects this code and writes its findings to docs/factory-prehook-worker.md as one short paragraph followed by exactly 3 bullet points? Do not read, modify, or mention docs/factory-preexisting-human-note.md. Do not create or edit the file in the main agent process. Only the Worker should write docs/factory-prehook-worker.md. Do not commit. Do not ask for confirmation.")
		s.WaitFor(t, session, s.Agent.PromptPattern(), 90*time.Second)

		// See TestFactoryTaskRecordExistsBeforeCommit: the file wait must
		// absorb the Worker's runtime because WaitFor can return mid-turn.
		testutil.WaitForFileExists(t, s.Dir, "docs/factory-prehook-worker.md", 120*time.Second)
		waitForCompletedTaskRecord(t, s.Dir, 30*time.Second)
		assertNoShadowTaskData(t, s.Dir)

		s.Git(t, "add", "docs/factory-prehook-worker.md")
		s.Git(t, "commit", "-m", "Add factory worker checkpoint regression fixtures")

		testutil.WaitForCheckpoint(t, s, 30*time.Second)
		cpID := testutil.AssertHasCheckpointTrailer(t, s.Dir, "HEAD")
		meta := testutil.WaitForSessionMetadata(t, s.Dir, cpID, 0, 30*time.Second)

		assert.Contains(t, meta.FilesTouched, "docs/factory-prehook-worker.md",
			"worker-created file should be tracked in committed checkpoint metadata")
		assert.NotContains(t, meta.FilesTouched, "docs/factory-preexisting-human-note.md",
			"pre-existing untracked sentinel should not leak into committed checkpoint metadata")
	})
}

// A Factory Worker's turn is captured as a COMPLETED session.TaskRecord on the
// parent's session state — not as task metadata on a shadow branch. #2032
// ("capture background subagent work durably") moved materialization to
// condensation time, so before the user commits there is a record and no
// shadow task tree; the record's transcript is written into the parent's
// checkpoint under tasks/<toolUseID>/ only once condensation runs.
//
// These two helpers pin that contract in both directions. The positive one
// replaced a poll for a shadow-branch tasks/ path, which asserted the
// pre-#2032 behaviour and so failed on every push to main from ed9c31c0d
// onwards; the negative one is the same assertion #2032 added to
// TestFactoryDroidWorkerSessionBecomesTaskCheckpoint ("a Worker's turn must
// write a task record, not shadow data"), narrowed to task data because a
// parent session legitimately has shadow branches of its own (shadow pinning
// keys on StepCount, so their existence says nothing about task content).
//
// Whether losing the pre-commit shadow copy weakens durability is #2058's
// question, not this test's: these assert the shipped contract.
func waitForCompletedTaskRecord(t *testing.T, dir string, timeout time.Duration) {
	t.Helper()

	stateDir := filepath.Join(dir, ".git", "entire-sessions")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(stateDir)
		if err != nil {
			// The directory may not exist yet; keep polling.
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") ||
				strings.HasSuffix(entry.Name(), ".tmp") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(stateDir, entry.Name()))
			if err != nil {
				continue
			}
			var state struct {
				TaskRecords []struct {
					ToolUseID   string `json:"tool_use_id"`
					CompletedAt string `json:"completed_at"`
				} `json:"task_records"`
			}
			if err := json.Unmarshal(data, &state); err != nil {
				continue
			}
			// An in-flight record has a zero CompletedAt, which omitempty drops
			// from the file entirely — so a non-empty value is the completion.
			for _, rec := range state.TaskRecords {
				if rec.ToolUseID != "" && rec.CompletedAt != "" {
					return
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("expected a completed task record in %s within %s", stateDir, timeout)
}

// assertNoShadowTaskData asserts no shadow branch carries task metadata.
func assertNoShadowTaskData(t *testing.T, dir string) {
	t.Helper()

	for _, branch := range testutil.ShadowBranches(t, dir) {
		out, err := testutil.GitOutputErr(dir, "ls-tree", "-r", "--name-only", branch)
		if err != nil {
			continue
		}
		for _, path := range strings.Split(out, "\n") {
			assert.NotContains(t, path, "/tasks/",
				"a Worker's turn must write a task record, not shadow task data (branch %s)", branch)
		}
	}
}
