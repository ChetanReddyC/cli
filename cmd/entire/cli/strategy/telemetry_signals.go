package strategy

import (
	"context"
	"os/exec"
	"regexp"
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

// searchProbe records whether the session consulted Entire's history search
// AND how that answer was obtained. The source travels with the boolean all the
// way to the payload because the two carry different information: "we looked and
// found nothing" and "we cannot look at this transcript" are both `used=false`,
// and a ratio computed over their union is a confident number over a population
// it silently cannot measure.
type searchProbe struct {
	used   bool
	source string
}

// Sources for searchProbe.source. Low cardinality on purpose — this is a
// PostHog property, and a new probe method should get a new label rather than
// being folded into an existing one.
const (
	// searchSourceUnsupported: this agent's transcript has no tool-call view,
	// so the question is unanswerable. used is false, and MUST NOT be read as
	// "did not search".
	searchSourceUnsupported = "unsupported"
	// searchSourceNone: the transcript was walked and no invocation was found.
	// This is the only trustworthy false.
	searchSourceNone = "none"
	// searchSourceCommand: a shell tool ran the command directly.
	searchSourceCommand = "command"
	// searchSourceSubagent: the entire-search subagent was dispatched.
	searchSourceSubagent = "subagent"
)

// entireSearchHints is the byte-level prefilter handed to the scanner. It is a
// performance filter, so every string entireSearchCommandPattern or the
// subagent check can accept must contain one of these LITERALLY — which is why
// the pattern below spells the internal separators as single spaces instead of
// \s+. TestSearchHintsCoverPattern pins the relationship; loosening the pattern
// without extending the hints is a silent false negative, not a slow path.
//
//nolint:gochecknoglobals // immutable lookup table, built once.
var entireSearchHints = [][]byte{
	[]byte("entire search"),
	[]byte("entire checkpoint search"),
	[]byte("entire-search"),
}

// entireSearchCommandPattern matches `entire search` / `entire checkpoint
// search` in COMMAND position — at the start of a command, or after a shell
// separator, tolerating leading env assignments and a path prefix. Position is
// the whole point: it accepts `cd sub && entire search "x" --json` and
// `/usr/local/bin/entire search x`, while rejecting the mentions that made the
// old substring probe ~18x false-positive, because in every one of them the
// phrase sits inside an argument rather than at a command boundary —
// `grep -rn "entire search" cmd/`, `git commit -m "... entire search ..."`.
//
// Known residual false negatives, and they are the safer direction: `xargs
// entire search`, `for f in …; do entire search; done` reached through a loop
// variable, and anything behind a wrapper script. They report `none`, so the
// missed-opportunity rate reads as an upper bound.
//
//nolint:gochecknoglobals // compiled once, read-only.
var entireSearchCommandPattern = regexp.MustCompile(
	`(?:^|[;&|(\n])\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)*(?:[\w./-]*/)?entire (?:checkpoint )?search(?:\s|$)`)

// entireSearchSubagent is the subagent name setup_search_skill.go scaffolds
// into .claude/agents/entire-search.md (and the Codex/Gemini equivalents).
//
// Matching the dispatch is not a nicety, it is the primary path. A session that
// consults search the way Entire ships it dispatches this subagent, and the
// subagent's own `entire search` Bash call is written to a SEPARATE transcript
// file that condensation never reads. Match only shell commands and the probe
// reports "did not search" for exactly the sessions that adopted the feature —
// a false negative aimed at the users we most want to count.
const entireSearchSubagent = "entire-search"

// detectSearchUsage reports whether the session consulted Entire's history
// search, and how it knows.
//
// Structural by construction: it asks the agent for recorded tool invocations
// rather than scanning bytes, so a transcript that merely *mentions* the command
// no longer counts. Entire installs the artifacts that made that distinction
// urgent — setup_search_skill.go embeds `entire search --json` in the search
// skill's own body, and investigate/prompt.go injects it into every investigate
// prompt — so the old probe fired on sessions that had only read Entire's own
// text.
//
// An empty transcript reports unsupported, not none: seeing nothing because
// there is nothing to see is not evidence that the session did not search.
func detectSearchUsage(ag agent.Agent, transcriptData []byte) searchProbe {
	source := searchSourceNone
	found, supported := agent.ScanToolInvocations(ag, transcriptData, entireSearchHints, func(inv agent.ToolInvocation) bool {
		if strings.EqualFold(strings.TrimSpace(inv.SubagentType), entireSearchSubagent) {
			source = searchSourceSubagent
			return true
		}
		if inv.Command != "" && entireSearchCommandPattern.MatchString(inv.Command) {
			source = searchSourceCommand
			return true
		}
		return false
	})
	if !supported {
		return searchProbe{used: false, source: searchSourceUnsupported}
	}
	return searchProbe{used: found, source: source}
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

// commitCondensedSignal captures, while the session gate is held, the few
// state/result fields the condensed-checkpoint signal needs. Everything
// expensive — the env/settings gates, the git-log density probe, machine-ID
// lookup, and the detached-process spawn — runs later in
// emitCommitCondensedTelemetry, after MutateSessionState has released the
// gate, matching the skill-event telemetry pattern.
type commitCondensedSignal struct {
	agentType    types.AgentType
	searchProbe  searchProbe
	filesTouched []string
}

// newCommitCondensedSignal snapshots the signal inputs for a successful
// condensation, or nil when there is nothing to report. Cheap and I/O-free by
// design — it is the only part of this signal that runs under the session
// gate. FilesTouched is copied because the caller mutates state after
// condensing.
//
// Commit-scoped by construction: the sole caller is condenseAndUpdateState,
// reached only from postCommitProcessSessionLocked. That is a scoping decision,
// not an oversight. The payload describes a commit — files_committed counts its
// files, and prior_ai_history's git-log probe passes --skip=1 precisely to
// exclude the commit just made — and it feeds the ratio "commits that landed
// AI-dense files without consulting search". The other condensation paths
// (CondenseSessionByID via doctor, CondenseAndMarkFullyCondensed at session
// end) condense real checkpoints but have no commit: --skip=1 would exclude an
// unrelated HEAD, and the resulting rows would be indistinguishable from
// genuine misses, inflating the denominator instead of completing it. Covering
// them means a trigger discriminator plus nullable commit-scoped fields — a
// change to what the metric means, not a bug fix.
func newCommitCondensedSignal(state *SessionState, result *CondenseResult) *commitCondensedSignal {
	if result == nil || result.Skipped || state == nil {
		return nil
	}
	files := make([]string, len(result.FilesTouched))
	copy(files, result.FilesTouched)
	return &commitCondensedSignal{
		agentType:    state.AgentType,
		searchProbe:  result.SearchProbe,
		filesTouched: files,
	}
}

// emitCommitCondensedTelemetry sends the content-free adoption signal for
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
func emitCommitCondensedTelemetry(ctx context.Context, sig *commitCondensedSignal) {
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
	// Send used_search only when it was actually measurable. An unsupported
	// transcript sends the source alone, so a consumer's `used_search = false`
	// filter excludes those rows instead of counting them as "did not search" —
	// a missing PostHog property is not false.
	var usedSearch *bool
	if sig.searchProbe.source != searchSourceUnsupported {
		used := sig.searchProbe.used
		usedSearch = &used
	}
	telemetry.TrackCommitCondensedDetached(telemetry.CommitCondensedSignal{
		Agent:            agentName,
		UsedSearch:       usedSearch,
		UsedSearchSource: sig.searchProbe.source,
		PriorAIHistory:   priorAIHistory,
		FilesCommitted:   len(sig.filesTouched),
	}, s.Enabled, versioninfo.Version)
}
