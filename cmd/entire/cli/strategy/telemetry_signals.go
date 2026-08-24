package strategy

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
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

// measured reports whether used is a real measurement rather than "we could not
// look".
//
// Deliberately an allowlist over the known-measured sources, not `source !=
// searchSourceUnsupported`. The zero value of searchProbe has source "", which a
// denylist admits — so any path that condenses without ever running the probe
// would ship used_search=false with a blank label: the fabricated negative this
// whole tri-state exists to prevent, wearing no label to reveal itself. An
// allowlist makes forgetting to set the probe fail safe instead.
func (p searchProbe) measured() bool {
	switch p.source {
	case searchSourceNone, searchSourceCommand, searchSourceSubagent:
		return true
	default:
		return false
	}
}

// label is the value sent as used_search_source. It maps the zero value onto
// unsupported so the property honours its documented contract of always being
// one of the four named sources.
func (p searchProbe) label() string {
	if p.source == "" {
		return searchSourceUnsupported
	}
	return p.source
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
		// Exact, not EqualFold: the hint prefilter that decides whether this
		// line is parsed at all is case-sensitive bytes.Contains, so a
		// case-insensitive matcher here would accept a spelling the prefilter
		// already discarded — a silent false negative, which is exactly the
		// hazard ToolInvocationScanner's doc warns callers about.
		if strings.TrimSpace(inv.SubagentType) == entireSearchSubagent {
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

// priorAICommitFiles returns the repo-root-relative paths touched by recent
// commits carrying an Entire checkpoint trailer, excluding the commit that was
// just created (--skip=1 — the commit being reported on is not its own prior
// history). Paths match git log --name-only output, i.e. the same form as
// SessionState.FilesTouched. Best-effort: any failure yields nil, read as "no
// prior history".
//
// A set rather than a predicate because the answer is commit-scoped and
// identical for every session in the commit — only the intersection differs —
// which is what lets commitCondensedEmitter run the subprocess once.
//
// -z keeps names unquoted and NUL-terminated, so non-ASCII paths (which git
// would otherwise emit quoted, e.g. "caf\303\251.go") still match their
// FilesTouched form, and names containing newlines survive parsing.
//
// Merge commits contribute nothing, and that is deliberate rather than a gap.
// --name-only emits no names for a merge, and the obvious fix
// (--diff-merges=first-parent) would make a "Merge origin/main into X" commit
// attribute every file it merged in, inflating prior_ai_history and therefore
// the miss rate. A merge's content is already attributed to the individual
// trailer-carrying commits it brings in, which sit in this window on their own
// account. Squash merges are single-parent, so their files do appear.
func priorAICommitFiles(ctx context.Context, repoRoot string) map[string]struct{} {
	// Each record starts with an empty field: -z NUL-terminates the format
	// output, and %x00 prefixes it, so splitting the whole output on NUL yields
	// ["", "<hash>\n<body>", "\n<file>", "<file>", …] per commit. A commit
	// message cannot contain NUL, so the record marker rests on nothing about
	// message content — the defect in the %x1e/%x1f framing this replaces,
	// where either control character in a body split a record early and dropped
	// a real trailer. %H guarantees the post-marker field is non-empty, so an
	// empty-message commit cannot produce two adjacent markers and mis-frame
	// the next record.
	out, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "log", "-z",
		"--skip=1", "-n", strconv.Itoa(priorAICheckpointsLookback),
		"--name-only", "--format=%x00%H%n%B").Output()
	if err != nil {
		return nil
	}
	var files map[string]struct{}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		if fields[i] != "" {
			continue // not a record marker; consumed below
		}
		i++
		if i >= len(fields) {
			break
		}
		isCheckpoint := false
		if _, ok := trailers.ParseCheckpoint(fields[i]); ok {
			isCheckpoint = true
		}
		// Consume this record's file list, whether or not we want it, so the
		// outer loop resumes on the next marker rather than mid-record.
		for i+1 < len(fields) && fields[i+1] != "" {
			i++
			if !isCheckpoint {
				continue
			}
			// The diff section opens with the newline separating it from the
			// format block; strip it from the segment carrying it.
			name := strings.TrimPrefix(fields[i], "\n")
			if name == "" {
				continue
			}
			if files == nil {
				files = make(map[string]struct{})
			}
			files[name] = struct{}{}
		}
	}
	return files
}

// commitCondensedSignal captures, while the session gate is held, the few
// state/result fields the condensed-checkpoint signal needs. Everything
// expensive — the env/settings gates, the git-log density probe, machine-ID
// lookup, and the detached-process spawn — runs later in
// commitCondensedEmitter.emit, after MutateSessionState has released the gate,
// matching the skill-event telemetry pattern.
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

// commitCondensedEmitter emits the cli_commit_condensed signal for each session
// of one commit, memoizing everything that is commit-scoped rather than
// session-scoped: the telemetry gate (settings load) and the git-log scan of
// prior AI checkpoint history.
//
// The scan was per session, and its cost is almost entirely process spawn:
// measured 13.8ms p50, flat across a 60x range of output size, against 8.7ms
// for `git --version` alone. Its output is identical for every session in the
// commit — only the per-session intersection differs — so paying it per session
// was paying for spawns, not work.
//
// Everything resolves on FIRST USE, and the order in emit is the promise: a
// commit where no session condenses runs neither settings.Load nor git log, and
// an opted-out user never reaches the probe. Laziness comes from where emit is
// called, not from this type — do not front-run the gate by resolving anything
// in the constructor.
//
// Not safe for concurrent use: PostCommit's session loop is sequential.
type commitCondensedEmitter struct {
	repoRoot string

	gateResolved bool
	// settings is non-nil only when the gate is open; s.Enabled is needed at
	// send time, which is why the loaded value is retained rather than reduced
	// to a bool.
	settings *settings.EntireSettings

	probed     bool
	priorFiles map[string]struct{}

	// probeFn is a test seam, like emitSkillTelemetry, so tests can count probe
	// invocations without spawning git.
	probeFn func(ctx context.Context, repoRoot string) map[string]struct{}
}

// newCommitCondensedEmitter returns an emitter for one commit. repoRoot is the
// worktree root PostCommit already resolved, which is what the per-session emit
// used to recompute.
func newCommitCondensedEmitter(repoRoot string) *commitCondensedEmitter {
	return &commitCondensedEmitter{repoRoot: repoRoot, probeFn: priorAICommitFiles}
}

// emit sends the content-free adoption signal for one condensed checkpoint: did
// the session consult search, and did the files it committed already carry AI
// checkpoint history? Together these give the "commits that landed AI-dense
// files without consulting search" ratio that raw command counts cannot.
//
// Gated on the env opt-out and then the opt-in telemetry setting before any
// probe work, and best-effort throughout: the PostHog call happens in a
// detached child and never blocks the hook. Call it AFTER the surrounding
// MutateSessionState returns, never inside its closure.
func (e *commitCondensedEmitter) emit(ctx context.Context, sig *commitCondensedSignal) {
	if e == nil || sig == nil {
		return
	}
	if telemetry.IsEnvOptedOut() {
		return
	}
	s, ok := e.allowed(ctx)
	if !ok {
		return
	}
	priorAIHistory := e.priorAITouched(ctx, sig.filesTouched)
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
	if sig.searchProbe.measured() {
		used := sig.searchProbe.used
		usedSearch = &used
	}
	emitCommitCondensed(telemetry.CommitCondensedSignal{
		Agent:            agentName,
		UsedSearch:       usedSearch,
		UsedSearchSource: sig.searchProbe.label(),
		PriorAIHistory:   priorAIHistory,
		FilesCommitted:   len(sig.filesTouched),
	}, s.Enabled, versioninfo.Version)
}

// allowed resolves the opt-in telemetry gate at most once per commit.
//
// IsTelemetryEnabled is #2023's helper, extracted precisely to stop the
// hand-rolled `s.Telemetry == nil || !*s.Telemetry` being copied a fourth time.
func (e *commitCondensedEmitter) allowed(ctx context.Context) (*settings.EntireSettings, bool) {
	if !e.gateResolved {
		e.gateResolved = true
		if s, err := settings.Load(ctx); err == nil && s.IsTelemetryEnabled() {
			e.settings = s
		}
	}
	return e.settings, e.settings != nil
}

// priorAITouched intersects one session's committed files against the commit's
// prior-history set, running the git-log scan at most once.
func (e *commitCondensedEmitter) priorAITouched(ctx context.Context, files []string) bool {
	if len(files) == 0 {
		return false
	}
	if !e.probed {
		e.probed = true
		e.priorFiles = e.probeFn(ctx, e.repoRoot)
	}
	for _, f := range files {
		if _, hit := e.priorFiles[f]; hit {
			return true
		}
	}
	return false
}

// emitCommitCondensed is the send step, separated from the gating above so
// tests can assert what the gate lets through without a PostHog client.
//
//nolint:gochecknoglobals // test seam, set and restored by in-package tests.
var emitCommitCondensed = telemetry.TrackCommitCondensedDetached
