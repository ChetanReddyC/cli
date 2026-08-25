package cli

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDoctorCodexWarningsNamePathOwnershipAndUserRemedies(t *testing.T) {
	t.Parallel()

	t.Run("linked worktree mismatch", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		writeCodexInactiveWorktreeWarning(
			&output,
			"/repo-feature/.codex/hooks.json",
			"/repo-main/.codex/hooks.json",
		)

		out := output.String()
		require.Contains(t, out, "NOT ACTIVE IN THIS WORKTREE")
		require.Contains(t, out, "/repo-feature/.codex/hooks.json")
		require.Contains(t, out, "/repo-main/.codex/hooks.json")
		require.Contains(t, out, "Commit .codex/hooks.json and apply that commit to the primary checkout")
		require.Contains(t, out, "run `entire enable` from the primary checkout")
		require.NotContains(t, out, "migrate")
		require.NotContains(t, out, "synchronize")
	})

	t.Run("invalid discovered file", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		writeCodexInvalidDiscoveredWarning(
			&output,
			"/repo-main/.codex/hooks.json",
			errors.New("Codex hooks file exceeds 1048576 bytes"),
		)

		out := output.String()
		require.Contains(t, out, "/repo-main/.codex/hooks.json")
		require.Contains(t, out, "exceeds 1048576 bytes")
		require.Contains(t, out, "Entire will not modify it from this worktree")
	})

	t.Run("missing project layer", func(t *testing.T) {
		t.Parallel()
		var output bytes.Buffer
		writeCodexMissingProjectLayerWarning(
			&output,
			"/repo-feature/.codex",
			"/repo-main/.codex/hooks.json",
		)

		out := output.String()
		require.Contains(t, out, "/repo-feature/.codex (missing)")
		require.Contains(t, out, "/repo-main/.codex/hooks.json")
		require.Contains(t, out, "will not copy or rewrite hooks in the other checkout")
	})
}

func TestCodexStatusWarningBehavior(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		issue *codexHookIssue
		want  string
	}{
		{
			name: "linked worktree mismatch",
			issue: &codexHookIssue{
				State:          codexHookStateInactiveWorktreePath,
				WorktreePath:   "/repo-feature/.codex/hooks.json",
				DiscoveredPath: "/repo-main/.codex/hooks.json",
			},
			want: "not active in this worktree",
		},
		{
			name:  "invalid discovery",
			issue: &codexHookIssue{State: codexHookStateInvalidDiscovered},
			want:  "discovered hooks are invalid",
		},
		{
			name:  "project layer",
			issue: &codexHookIssue{State: codexHookStateProjectLayerMissing},
			want:  "project layer missing",
		},
		{
			name:  "trust gaps",
			issue: &codexHookIssue{State: codexHookStateTrustReview, MissingApprovals: []string{"stop", "post_tool_use"}},
			want:  "2 Codex hook(s) need approval · open /hooks",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Contains(t, codexStatusWarning(tt.issue), tt.want)
		})
	}
}

func TestCodexSessionStartWarningsStayConcise(t *testing.T) {
	t.Parallel()

	mismatch := codexSessionStartWarning(&codexHookIssue{State: codexHookStateInactiveWorktreePath})
	require.Equal(t, "Entire hooks in this worktree are not active; Codex discovers another checkout. Run 'entire doctor'.", mismatch)
	require.NotContains(t, mismatch, "/repo-main")

	trust := codexSessionStartWarning(&codexHookIssue{
		State:            codexHookStateTrustReview,
		MissingApprovals: []string{"post_tool_use", "subagent_start"},
	})
	require.Equal(t, "2 Codex hook(s) await approval. Open /hooks.", trust)
	require.NotContains(t, trust, "trusted_hash")
}

func TestCodexHooksStatusJSONPreservesDiagnosticPaths(t *testing.T) {
	t.Parallel()
	status := codexHooksStatusFromIssue(&codexHookIssue{
		State:            codexHookStateInactiveWorktreePath,
		WorktreePath:     "/repo-feature/.codex/hooks.json",
		DiscoveredPath:   "/repo-main/.codex/hooks.json",
		MissingApprovals: []string{"post_tool_use"},
	})

	require.Equal(t, codexHookStateInactiveWorktreePath, status.State)
	require.Equal(t, "/repo-feature/.codex/hooks.json", status.WorktreePath)
	require.Equal(t, "/repo-main/.codex/hooks.json", status.DiscoveredPath)
	require.Equal(t, []string{"post_tool_use"}, status.MissingApprovals)
}
