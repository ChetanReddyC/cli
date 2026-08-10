package strategy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"
)

// TestCondenseSession_CommittedCheckpointCarriesSubagentTokens pins the
// checkpoint-scoped subagent total onto the committed checkpoint's metadata.
//
// Condensation recomputes token usage from the transcript with subagentsDir="",
// which by contract leaves SubagentTokens nil, and that recomputed value overwrote
// the checkpoint-scoped total SaveStep had already rescoped into
// state.CheckpointTokenUsage. Result: committed checkpoints reported
// "subagent_tokens": null even for sessions that ran many subagents (0 of 30
// sampled real checkpoints carried one).
func TestCondenseSession_CommittedCheckpointCarriesSubagentTokens(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	ctx := context.Background()
	s := &ManualCommitStrategy{}
	sessionID := "2026-08-10-condense-subagent-tokens"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))

	// The assistant line carries real usage, so the transcript recompute fires and
	// produces main-agent tokens with no SubagentTokens — the overwrite this test
	// guards against.
	transcript := `{"type":"human","message":{"content":"delegate to a subagent"}}
{"type":"assistant","uuid":"a1","message":{"id":"m1","usage":{"input_tokens":300,"output_tokens":150}}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte("agent-modified"), 0o644))

	require.NoError(t, s.SaveStep(ctx, StepContext{
		SessionID:      sessionID,
		MetadataDir:    metadataDir,
		MetadataDirAbs: metadataDirAbs,
		ModifiedFiles:  []string{"test.txt"},
		CommitMessage:  "checkpoint 1",
		AuthorName:     "Test",
		AuthorEmail:    "test@test.com",
		AgentType:      agent.AgentTypeClaudeCode,
		TokenUsage: &agent.TokenUsage{
			InputTokens: 100, OutputTokens: 50, APICallCount: 1,
			SubagentTokens: &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5},
		},
	}))

	state, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.CheckpointTokenUsage.SubagentTokens,
		"precondition: SaveStep records the checkpoint-scoped subagent total")

	checkpointID := id.MustCheckpointID("aabbccdd3344")
	result, err := s.CondenseSession(ctx, repo, checkpointID, state, nil)
	require.NoError(t, err)
	require.False(t, result.Skipped, "condensation must not skip when files are touched")

	summary := readCheckpointSummary(t, repo, checkpointID)
	require.NotNil(t, summary.TokenUsage, "committed checkpoint must carry token usage")
	require.NotNil(t, summary.TokenUsage.SubagentTokens,
		"committed checkpoint must carry the checkpoint-scoped subagent total")
	require.Equal(t, 500, summary.TokenUsage.SubagentTokens.InputTokens)
	require.Equal(t, 250, summary.TokenUsage.SubagentTokens.OutputTokens)
	require.Equal(t, 5, summary.TokenUsage.SubagentTokens.APICallCount)

	// The session-wide cumulative must survive the fill untouched. The fill copies
	// rather than mutates precisely because state.TokenUsage can alias the
	// checkpoint usage (applyBackfilledSessionTokenUsage adopts it for Copilot CLI);
	// mutating in place would overwrite the cumulative with this window's delta and
	// make resetCheckpointWindow snapshot a too-small baseline for the next window.
	require.NotNil(t, state.TokenUsage.SubagentTokens,
		"session-wide cumulative subagent total must survive condensation")
	require.Equal(t, 500, state.TokenUsage.SubagentTokens.InputTokens)
	require.Equal(t, 250, state.TokenUsage.SubagentTokens.OutputTokens)
}

// TestCondenseSession_SubagentTokensStayScopedToCheckpointWindow covers the second
// checkpoint of a session: the committed value must be that window's delta, not the
// session-wide cumulative, so summing checkpoints does not over-count.
func TestCondenseSession_SubagentTokensStayScopedToCheckpointWindow(t *testing.T) {
	dir := setupGitRepo(t)
	t.Chdir(dir)

	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)

	ctx := context.Background()
	s := &ManualCommitStrategy{}
	sessionID := "2026-08-10-condense-subagent-window"

	metadataDir := ".entire/metadata/" + sessionID
	metadataDirAbs := filepath.Join(dir, metadataDir)
	require.NoError(t, os.MkdirAll(metadataDirAbs, 0o755))
	transcript := `{"type":"human","message":{"content":"delegate again"}}
{"type":"assistant","uuid":"a1","message":{"id":"m1","usage":{"input_tokens":300,"output_tokens":150}}}
`
	require.NoError(t, os.WriteFile(filepath.Join(metadataDirAbs, paths.TranscriptFileName), []byte(transcript), 0o644))

	saveStep := func(commitMsg, content string, subagent *agent.TokenUsage) {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "test.txt"), []byte(content), 0o644))
		require.NoError(t, s.SaveStep(ctx, StepContext{
			SessionID:      sessionID,
			MetadataDir:    metadataDir,
			MetadataDirAbs: metadataDirAbs,
			ModifiedFiles:  []string{"test.txt"},
			CommitMessage:  commitMsg,
			AuthorName:     "Test",
			AuthorEmail:    "test@test.com",
			AgentType:      agent.AgentTypeClaudeCode,
			TokenUsage: &agent.TokenUsage{
				InputTokens: 100, OutputTokens: 50, APICallCount: 1,
				SubagentTokens: subagent,
			},
		}))
	}

	// Checkpoint 1: subagent cumulative 500/250. Condense through
	// CondenseSessionByID so the real reset path runs and snapshots the baseline —
	// CondenseSession alone does not reset the window (its callers do).
	saveStep("checkpoint 1", "v2", &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5})
	require.NoError(t, s.CondenseSessionByID(ctx, sessionID))

	stateReset, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, stateReset.SubagentTokensBaseline, "precondition: reset snapshots the baseline")
	require.Equal(t, 500, stateReset.SubagentTokensBaseline.InputTokens)

	// Checkpoint 2: the subagent grew to 620/310 cumulative — a 120/60 delta.
	saveStep("checkpoint 2", "v3", &agent.TokenUsage{InputTokens: 620, OutputTokens: 310, APICallCount: 6})
	state2, err := s.loadSessionState(ctx, sessionID)
	require.NoError(t, err)
	secondID := id.MustCheckpointID("aabbccdd7788")
	_, err = s.CondenseSession(ctx, repo, secondID, state2, nil)
	require.NoError(t, err)

	summary := readCheckpointSummary(t, repo, secondID)
	require.NotNil(t, summary.TokenUsage)
	require.NotNil(t, summary.TokenUsage.SubagentTokens)
	require.Equal(t, 120, summary.TokenUsage.SubagentTokens.InputTokens,
		"second checkpoint must carry its window's delta, not the session cumulative")
	require.Equal(t, 60, summary.TokenUsage.SubagentTokens.OutputTokens)
}

// readCheckpointSummary reads a committed checkpoint's root CheckpointSummary off
// the metadata branch.
func readCheckpointSummary(t *testing.T, repo *git.Repository, checkpointID id.CheckpointID) checkpoint.CheckpointSummary {
	t.Helper()

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(paths.MetadataBranchName), true)
	require.NoError(t, err)
	commit, err := repo.CommitObject(ref.Hash())
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	checkpointTree, err := tree.Tree(checkpointID.Path())
	require.NoError(t, err)
	rootMeta, err := checkpointTree.File(paths.MetadataFileName)
	require.NoError(t, err)
	rootBytes, err := rootMeta.Contents()
	require.NoError(t, err)

	var summary checkpoint.CheckpointSummary
	require.NoError(t, json.Unmarshal([]byte(rootBytes), &summary))
	return summary
}
