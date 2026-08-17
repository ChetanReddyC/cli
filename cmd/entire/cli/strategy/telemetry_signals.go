package strategy

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/settings"
	"github.com/entireio/cli/cmd/entire/cli/telemetry"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
)

// priorAICheckpointsLookback bounds how many commits back the
// missed-opportunity signal scans for AI checkpoint history. One bounded git
// subprocess regardless of repo size.
const priorAICheckpointsLookback = 50

// transcriptMentionsEntireSearch reports whether the raw transcript contains
// an `entire search` (or legacy `entire checkpoint search`) invocation. A
// substring probe over agent-native JSONL is deliberately loose — a prompt
// merely *mentioning* the command also matches — which is acceptable for an
// aggregate telemetry boolean and never used for behavior.
func transcriptMentionsEntireSearch(transcript []byte) bool {
	return bytes.Contains(transcript, []byte("entire search")) ||
		bytes.Contains(transcript, []byte("entire checkpoint search"))
}

// priorAICommitTouchedFiles reports whether any of files was touched by a
// recent commit carrying an Entire checkpoint trailer, excluding the commit
// that was just created (--skip=1). files must be repo-root-relative, matching
// git log --name-only output. Best-effort: any failure reports false.
//
// Paths git quotes in --name-only output (e.g. non-ASCII names) won't match
// their unquoted FilesTouched form; that false-negative is acceptable for a
// telemetry boolean.
func priorAICommitTouchedFiles(ctx context.Context, repoRoot string, files []string) bool {
	if len(files) == 0 {
		return false
	}
	// \x1e separates commits; \x1f separates each commit's message from the
	// --name-only file list git appends after the format block.
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "log",
		"--skip=1", "-n", strconv.Itoa(priorAICheckpointsLookback),
		"--name-only", "--format=%x1e%B%x1f").Output()
	if err != nil {
		return false
	}
	fileSet := make(map[string]struct{}, len(files))
	for _, f := range files {
		fileSet[f] = struct{}{}
	}
	for _, record := range strings.Split(string(out), "\x1e") {
		message, fileList, found := strings.Cut(record, "\x1f")
		if !found {
			continue
		}
		if _, isCheckpoint := trailers.ParseCheckpoint(message); !isCheckpoint {
			continue
		}
		for _, line := range strings.Split(fileList, "\n") {
			name := strings.TrimSpace(line)
			if name == "" {
				continue
			}
			if _, hit := fileSet[name]; hit {
				return true
			}
		}
	}
	return false
}

// emitCheckpointCondensedTelemetry sends the content-free adoption signal for
// one condensed checkpoint: did the session consult search, and did the files
// it committed already carry AI checkpoint history? Together these give the
// "sessions that edited history-dense files without searching" denominator
// that raw command counts cannot.
//
// Gated on the opt-in telemetry setting before any work happens — the git-log
// density probe only runs when telemetry is on — and best-effort throughout:
// the PostHog call happens in a detached child and never blocks the hook.
func emitCheckpointCondensedTelemetry(ctx context.Context, state *SessionState, result *CondenseResult) {
	if result == nil || result.Skipped || state == nil {
		return
	}
	s, err := settings.Load(ctx)
	if err != nil || s.Telemetry == nil || !*s.Telemetry {
		return
	}
	priorAIHistory := false
	if root, rootErr := paths.WorktreeRoot(ctx); rootErr == nil {
		priorAIHistory = priorAICommitTouchedFiles(ctx, root, result.FilesTouched)
	}
	// Report the registry key ("claude-code"), not the display name stored in
	// state.AgentType ("Claude Code"), so the agent property lines up with the
	// skill and command events. Unknown agent types fall back to the stored
	// string rather than dropping the signal.
	agentName := string(state.AgentType)
	if ag, agErr := agent.GetByAgentType(state.AgentType); agErr == nil && ag != nil {
		agentName = string(ag.Name())
	}
	telemetry.TrackCheckpointCondensedDetached(telemetry.CheckpointCondensedSignal{
		Agent:          agentName,
		UsedSearch:     result.UsedSearch,
		PriorAIHistory: priorAIHistory,
		FilesCommitted: len(result.FilesTouched),
	}, s.Enabled, versioninfo.Version)
}
