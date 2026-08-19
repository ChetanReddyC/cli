package strategy

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
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
// -z keeps names unquoted and NUL-terminated, so non-ASCII paths (which git
// would otherwise emit quoted, e.g. "caf\303\251.go") still match their
// FilesTouched form, and names containing newlines survive parsing.
func priorAICommitTouchedFiles(ctx context.Context, repoRoot string, files []string) bool {
	if len(files) == 0 {
		return false
	}
	// \x1e separates commits; \x1f separates each commit's message from the
	// --name-only file list git appends after the format block.
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "log", "-z",
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
		for _, name := range strings.Split(fileList, "\x00") {
			// The diff section still opens with the newline separating it
			// from the format block; strip it from the segment carrying it.
			name = strings.TrimPrefix(name, "\n")
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

// condensedTelemetrySignal captures, while the session gate is held, the few
// state/result fields the condensed-checkpoint signal needs. Everything
// expensive — the env/settings gates, the git-log density probe, machine-ID
// lookup, and the detached-process spawn — runs later in
// emitCheckpointCondensedTelemetry, after MutateSessionState has released the
// gate, matching the skill-event telemetry pattern.
type condensedTelemetrySignal struct {
	agentType    types.AgentType
	usedSearch   bool
	filesTouched []string
}

// newCondensedTelemetrySignal snapshots the signal inputs for a successful
// condensation, or nil when there is nothing to report. Cheap and I/O-free by
// design — it is the only part of this signal that runs under the session
// gate. FilesTouched is copied because the caller mutates state after
// condensing.
func newCondensedTelemetrySignal(state *SessionState, result *CondenseResult) *condensedTelemetrySignal {
	if result == nil || result.Skipped || state == nil {
		return nil
	}
	files := make([]string, len(result.FilesTouched))
	copy(files, result.FilesTouched)
	return &condensedTelemetrySignal{
		agentType:    state.AgentType,
		usedSearch:   result.UsedSearch,
		filesTouched: files,
	}
}

// emitCheckpointCondensedTelemetry sends the content-free adoption signal for
// one condensed checkpoint: did the session consult search, and did the files
// it committed already carry AI checkpoint history? Together these give the
// "sessions that edited history-dense files without searching" denominator
// that raw command counts cannot.
//
// Gated on both the env opt-out and the opt-in telemetry setting before any
// work happens — an opted-out user never pays for the git-log density probe —
// and best-effort throughout: the PostHog call happens in a detached child and
// never blocks the hook. Call it AFTER the surrounding MutateSessionState
// returns, never inside its closure.
func emitCheckpointCondensedTelemetry(ctx context.Context, sig *condensedTelemetrySignal) {
	if sig == nil {
		return
	}
	if telemetry.IsEnvOptedOut() {
		return
	}
	s, err := settings.Load(ctx)
	if err != nil || s.Telemetry == nil || !*s.Telemetry {
		return
	}
	priorAIHistory := false
	if root, rootErr := paths.WorktreeRoot(ctx); rootErr == nil {
		priorAIHistory = priorAICommitTouchedFiles(ctx, root, sig.filesTouched)
	}
	// Report the registry key ("claude-code"), not the display name stored in
	// state.AgentType ("Claude Code"), so the agent property lines up with the
	// skill and command events. Unknown agent types fall back to the stored
	// string rather than dropping the signal.
	agentName := string(sig.agentType)
	if ag, agErr := agent.GetByAgentType(sig.agentType); agErr == nil && ag != nil {
		agentName = string(ag.Name())
	}
	telemetry.TrackCheckpointCondensedDetached(telemetry.CheckpointCondensedSignal{
		Agent:          agentName,
		UsedSearch:     sig.usedSearch,
		PriorAIHistory: priorAIHistory,
		FilesCommitted: len(sig.filesTouched),
	}, s.Enabled, versioninfo.Version)
}
