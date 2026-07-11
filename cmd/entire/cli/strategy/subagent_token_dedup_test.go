package strategy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/stretchr/testify/require"
)

// TestAccumulateTokenUsage_SubagentTokensReplacedNotSummed is a focused unit
// test on accumulateTokenUsage: CalculateTotalTokenUsage (claudecode and
// factoryaidroid) discovers subagent IDs from the full transcript and re-reads
// each subagent transcript from line 0 on every call, so incoming.SubagentTokens
// is always a cumulative-since-session-start snapshot, not a per-step delta.
// Summing that snapshot across steps (as accumulateTokenUsage does for the
// main-agent fields) would re-add a subagent's full usage on every subsequent
// step after it was first discovered. accumulateTokenUsage must replace
// SubagentTokens with the latest snapshot instead.
func TestAccumulateTokenUsage_SubagentTokensReplacedNotSummed(t *testing.T) {
	subagentSnapshot := &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5}

	step1 := &agent.TokenUsage{InputTokens: 100, OutputTokens: 50, APICallCount: 1, SubagentTokens: subagentSnapshot}
	existing := accumulateTokenUsage(nil, step1)
	require.NotNil(t, existing.SubagentTokens)
	require.Equal(t, 500, existing.SubagentTokens.InputTokens)
	require.Equal(t, 250, existing.SubagentTokens.OutputTokens)

	// Second step within the same checkpoint window: the subagent transcript
	// hasn't changed, so CalculateTotalTokenUsage returns the SAME cumulative
	// snapshot again. Main-agent fields are per-step deltas and should sum;
	// SubagentTokens must NOT double.
	step2 := &agent.TokenUsage{InputTokens: 100, OutputTokens: 50, APICallCount: 1, SubagentTokens: subagentSnapshot}
	existing = accumulateTokenUsage(existing, step2)

	require.Equal(t, 200, existing.InputTokens, "main-agent InputTokens should sum across steps")
	require.Equal(t, 100, existing.OutputTokens, "main-agent OutputTokens should sum across steps")
	require.NotNil(t, existing.SubagentTokens)
	require.Equal(t, 500, existing.SubagentTokens.InputTokens, "SubagentTokens must be replaced, not summed")
	require.Equal(t, 250, existing.SubagentTokens.OutputTokens, "SubagentTokens must be replaced, not summed")
}

// TestSaveStep_SubagentTokensNotDoubleCountedAcrossCheckpoints exercises the
// real SaveStep path for both Claude Code and Factory AI Droid (the two
// agents whose CalculateTotalTokenUsage implementations discover subagent IDs
// from the full transcript per #329) and proves that a subagent discovered
// before a checkpoint window is folded into that checkpoint's token usage
// exactly once, not re-added on every subsequent checkpoint it remains
// discoverable in.
func TestSaveStep_SubagentTokensNotDoubleCountedAcrossCheckpoints(t *testing.T) {
	agentTypes := []types.AgentType{agent.AgentTypeClaudeCode, agent.AgentTypeFactoryAIDroid}

	for _, agentType := range agentTypes {
		t.Run(string(agentType), func(t *testing.T) {
			dir := t.TempDir()
			testutil.InitRepo(t, dir)
			repo, err := git.PlainOpen(dir)
			require.NoError(t, err)

			worktree, err := repo.Worktree()
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v1"), 0o644))
			_, err = worktree.Add("test.txt")
			require.NoError(t, err)
			_, err = worktree.Commit("Initial commit", &git.CommitOptions{
				Author: &object.Signature{Name: "Test", Email: "test@test.com", When: time.Now()},
			})
			require.NoError(t, err)

			t.Chdir(dir)
			ctx := context.Background()
			s := &ManualCommitStrategy{}
			sessionID := "2026-07-10-subagent-dedup-" + string(agentType)

			metadataDir := ".entire/metadata/" + sessionID
			metadataDirAbs := filepath.Join(dir, metadataDir)
			require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
			transcript := `{"type":"human","message":{"content":"test"}}` + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

			// Checkpoint 1, step 1: a subagent spawned before this checkpoint's
			// window is discovered via the full-transcript scan (#329) and its
			// cumulative usage as of now is 500/250 across 5 calls.
			subagentAtCheckpoint1 := &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5}
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:      sessionID,
				MetadataDir:    metadataDir,
				MetadataDirAbs: metadataDirAbs,
				ModifiedFiles:  []string{"test.txt"},
				CommitMessage:  "checkpoint 1 step 1",
				AuthorName:     "Test",
				AuthorEmail:    "test@test.com",
				AgentType:      agentType,
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100, OutputTokens: 50, APICallCount: 1,
					SubagentTokens: subagentAtCheckpoint1,
				},
			}))

			// Checkpoint 1, step 2: same turn window, subagent transcript
			// unchanged (CalculateTotalTokenUsage would return the identical
			// cumulative snapshot again since it always re-reads from line 0).
			// Change the working tree so SaveStep sees a real diff to save.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v2"), 0o644))
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:      sessionID,
				MetadataDir:    metadataDir,
				MetadataDirAbs: metadataDirAbs,
				ModifiedFiles:  []string{"test.txt"},
				CommitMessage:  "checkpoint 1 step 2",
				AuthorName:     "Test",
				AuthorEmail:    "test@test.com",
				AgentType:      agentType,
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100, OutputTokens: 50, APICallCount: 1,
					SubagentTokens: subagentAtCheckpoint1,
				},
			}))

			state, err := s.loadSessionState(ctx, sessionID)
			require.NoError(t, err)
			require.NotNil(t, state.CheckpointTokenUsage)
			require.NotNil(t, state.CheckpointTokenUsage.SubagentTokens)
			require.Equal(t, 500, state.CheckpointTokenUsage.SubagentTokens.InputTokens,
				"subagent usage must be folded once per checkpoint window, not once per step")
			require.Equal(t, 250, state.CheckpointTokenUsage.SubagentTokens.OutputTokens)
			require.Equal(t, 200, state.CheckpointTokenUsage.InputTokens, "main-agent deltas still sum across steps")

			// Simulate the condensation reset that happens between checkpoints:
			// CheckpointTokenUsage is cleared and SubagentTokensBaseline snapshots
			// the cumulative subagent total counted so far, so the next
			// checkpoint's CheckpointTokenUsage.SubagentTokens is scoped to
			// "since this reset" instead of the whole session again.
			require.NoError(t, MutateSessionState(ctx, sessionID, func(st *SessionState) error {
				st.StepCount = 0
				st.CheckpointTokenUsage = nil
				if st.TokenUsage != nil {
					st.SubagentTokensBaseline = st.TokenUsage.SubagentTokens
				}
				st.CheckpointTranscriptStart = 10
				return nil
			}))

			// Checkpoint 2, step 1: the same subagent is still discoverable (its
			// marker line is still in the full transcript) and has grown a bit
			// more since checkpoint 1.
			subagentAtCheckpoint2 := &agent.TokenUsage{InputTokens: 620, OutputTokens: 310, APICallCount: 6}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("v3"), 0o644))
			require.NoError(t, s.SaveStep(ctx, StepContext{
				SessionID:      sessionID,
				MetadataDir:    metadataDir,
				MetadataDirAbs: metadataDirAbs,
				ModifiedFiles:  []string{"test.txt"},
				CommitMessage:  "checkpoint 2 step 1",
				AuthorName:     "Test",
				AuthorEmail:    "test@test.com",
				AgentType:      agentType,
				TokenUsage: &agent.TokenUsage{
					InputTokens: 100, OutputTokens: 50, APICallCount: 1,
					SubagentTokens: subagentAtCheckpoint2,
				},
			}))

			state2, err := s.loadSessionState(ctx, sessionID)
			require.NoError(t, err)

			// The session-wide total tracks the latest cumulative subagent
			// snapshot directly (it is already cumulative) — not the sum of the
			// checkpoint-1 and checkpoint-2 snapshots.
			require.NotNil(t, state2.TokenUsage.SubagentTokens)
			require.Equal(t, 620, state2.TokenUsage.SubagentTokens.InputTokens,
				"session-wide subagent total must be the latest cumulative snapshot, not summed across checkpoints")
			require.Equal(t, 310, state2.TokenUsage.SubagentTokens.OutputTokens)

			// Checkpoint 2's own CheckpointTokenUsage.SubagentTokens must be
			// rescoped to just what grew since the checkpoint-1 baseline
			// (620-500, 310-250), not the full cumulative total again.
			require.NotNil(t, state2.CheckpointTokenUsage)
			require.NotNil(t, state2.CheckpointTokenUsage.SubagentTokens)
			require.Equal(t, 120, state2.CheckpointTokenUsage.SubagentTokens.InputTokens,
				"checkpoint 2's subagent delta must exclude what was already counted in checkpoint 1")
			require.Equal(t, 60, state2.CheckpointTokenUsage.SubagentTokens.OutputTokens)
		})
	}
}
