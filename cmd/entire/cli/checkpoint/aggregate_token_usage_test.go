package checkpoint

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// TestAggregateTokenUsage_SumsSubagentTokens pins the nested SubagentTokens into
// the root CheckpointSummary. The aggregation copied only the five scalar fields,
// so a checkpoint's root metadata.json reported "subagent_tokens": null even when
// its per-session metadata carried one.
//
// Summing is correct here: this aggregates across the *sessions* of one checkpoint,
// and each session's SubagentTokens is already that session's checkpoint-scoped
// total. (The replace-don't-add rule applies to accumulating steps within a
// session, where each step re-reports a cumulative snapshot.)
func TestAggregateTokenUsage_SumsSubagentTokens(t *testing.T) {
	t.Parallel()

	t.Run("single session", func(t *testing.T) {
		t.Parallel()
		got := aggregateTokenUsage(nil, &agent.TokenUsage{
			InputTokens: 300, OutputTokens: 150, APICallCount: 2,
			SubagentTokens: &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5},
		})
		require.NotNil(t, got.SubagentTokens)
		require.Equal(t, 500, got.SubagentTokens.InputTokens)
		require.Equal(t, 250, got.SubagentTokens.OutputTokens)
		require.Equal(t, 5, got.SubagentTokens.APICallCount)
	})

	t.Run("two sessions both with subagents", func(t *testing.T) {
		t.Parallel()
		a := &agent.TokenUsage{
			InputTokens: 300, OutputTokens: 150,
			SubagentTokens: &agent.TokenUsage{InputTokens: 500, OutputTokens: 250, APICallCount: 5},
		}
		b := &agent.TokenUsage{
			InputTokens: 100, OutputTokens: 40,
			SubagentTokens: &agent.TokenUsage{InputTokens: 20, OutputTokens: 10, APICallCount: 1},
		}
		got := aggregateTokenUsage(a, b)
		require.Equal(t, 400, got.InputTokens)
		require.Equal(t, 190, got.OutputTokens)
		require.NotNil(t, got.SubagentTokens)
		require.Equal(t, 520, got.SubagentTokens.InputTokens)
		require.Equal(t, 260, got.SubagentTokens.OutputTokens)
		require.Equal(t, 6, got.SubagentTokens.APICallCount)
	})

	t.Run("only one session has subagents", func(t *testing.T) {
		t.Parallel()
		got := aggregateTokenUsage(
			&agent.TokenUsage{InputTokens: 300},
			&agent.TokenUsage{InputTokens: 100, SubagentTokens: &agent.TokenUsage{InputTokens: 20}},
		)
		require.NotNil(t, got.SubagentTokens)
		require.Equal(t, 20, got.SubagentTokens.InputTokens)
	})

	t.Run("no subagents stays nil", func(t *testing.T) {
		t.Parallel()
		got := aggregateTokenUsage(&agent.TokenUsage{InputTokens: 300}, &agent.TokenUsage{InputTokens: 100})
		require.Nil(t, got.SubagentTokens, "must not synthesize an empty subagent total")
	})

	t.Run("both nil", func(t *testing.T) {
		t.Parallel()
		require.Nil(t, aggregateTokenUsage(nil, nil))
	})
}
