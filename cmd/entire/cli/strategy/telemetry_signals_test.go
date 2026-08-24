package strategy

import (
	"bytes"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

// Real Claude Code JSONL lines. The shapes matter: the old substring probe was
// tested against invented ones ({"tool":"Bash","command":...}) that no agent
// writes, which is part of how an 18-to-1 false-positive rate went unnoticed.
const (
	fixtureBashSearch = `{"type":"assistant","uuid":"a1","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"entire search \"retry backoff\" --json"}}]}}
`
	fixtureBashCheckpointSearch = `{"type":"assistant","uuid":"a2","message":{"content":[{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"entire checkpoint search foo"}}]}}
`
	fixtureBashChainedSearch = `{"type":"assistant","uuid":"a3","message":{"content":[{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"cd sub && entire search foo --json"}}]}}
`
	fixtureSubagentDispatch = `{"type":"assistant","uuid":"a4","message":{"content":[{"type":"tool_use","id":"t4","name":"Agent","input":{"subagent_type":"entire-search","prompt":"find prior work on retries"}}]}}
`
	fixtureBashGrepMention = `{"type":"assistant","uuid":"a5","message":{"content":[{"type":"tool_use","id":"t5","name":"Bash","input":{"command":"grep -rn \"entire search\" cmd/"}}]}}
`
	fixtureBashCommitMessage = `{"type":"assistant","uuid":"a6","message":{"content":[{"type":"tool_use","id":"t6","name":"Bash","input":{"command":"git commit -m \"mention entire search here\""}}]}}
`
	fixtureToolResultOutput = `{"type":"user","uuid":"u1","message":{"content":[{"type":"tool_result","tool_use_id":"t9","content":"$ entire search foo\nno results"}]}}
`
	fixtureAssistantTextMention = `{"type":"assistant","uuid":"a7","message":{"content":[{"type":"text","text":"You could run entire search to find that."}]}}
`
	fixtureWriteSkillBody = `{"type":"assistant","uuid":"a8","message":{"content":[{"type":"tool_use","id":"t8","name":"Write","input":{"file_path":".claude/agents/entire-search.md","content":"Your only history-search mechanism is the entire search --json command."}}]}}
`
	fixtureInvestigatePrompt = `{"type":"user","uuid":"u2","message":{"content":"Run entire search \"<phrase from the symptom>\" --json to find prior work."}}
`
	fixtureUnrelatedBash = `{"type":"assistant","uuid":"a9","message":{"content":[{"type":"tool_use","id":"t7","name":"Bash","input":{"command":"entire status"}}]}}
`
)

func TestDetectSearchUsage_ClaudeCode(t *testing.T) {
	t.Parallel()

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}

	tests := []struct {
		name       string
		transcript string
		want       searchProbe
	}{
		{"bash invocation", fixtureBashSearch, searchProbe{used: true, source: searchSourceCommand}},
		{"checkpoint search alias", fixtureBashCheckpointSearch, searchProbe{used: true, source: searchSourceCommand}},
		{"after a shell separator", fixtureBashChainedSearch, searchProbe{used: true, source: searchSourceCommand}},
		{"entire-search subagent dispatch", fixtureSubagentDispatch, searchProbe{used: true, source: searchSourceSubagent}},

		// The false-positive classes the substring probe could not tell apart
		// from a real invocation. Each of these is a documented source: Entire's
		// own search skill body, the investigate prompt, or a session reading
		// this repository.
		{"grep for the phrase", fixtureBashGrepMention, searchProbe{used: false, source: searchSourceNone}},
		{"phrase inside a commit message", fixtureBashCommitMessage, searchProbe{used: false, source: searchSourceNone}},
		{"phrase in command output", fixtureToolResultOutput, searchProbe{used: false, source: searchSourceNone}},
		{"phrase in assistant prose", fixtureAssistantTextMention, searchProbe{used: false, source: searchSourceNone}},
		{"scaffolded skill body being written", fixtureWriteSkillBody, searchProbe{used: false, source: searchSourceNone}},
		{"injected investigate prompt", fixtureInvestigatePrompt, searchProbe{used: false, source: searchSourceNone}},

		{"unrelated entire command", fixtureUnrelatedBash, searchProbe{used: false, source: searchSourceNone}},
		{"phrase absent", `{"type":"assistant","uuid":"a0","message":{"content":[]}}`, searchProbe{used: false, source: searchSourceNone}},

		// Not "none": seeing nothing because there is nothing to see is a
		// different fact from having looked.
		{"empty transcript", "", searchProbe{used: false, source: searchSourceUnsupported}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := detectSearchUsage(ag, []byte(tt.transcript)); got != tt.want {
				t.Errorf("detectSearchUsage() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestDetectSearchUsage_MixedTranscriptFindsTheInvocation guards the walk
// itself: the accepting line must be found among rejected ones, in either
// order, so a prefilter miss cannot masquerade as "did not search".
func TestDetectSearchUsage_MixedTranscriptFindsTheInvocation(t *testing.T) {
	t.Parallel()

	ag, err := agent.GetByAgentType(agent.AgentTypeClaudeCode)
	if err != nil {
		t.Fatalf("GetByAgentType(ClaudeCode) error: %v", err)
	}
	noise := fixtureToolResultOutput + fixtureAssistantTextMention + fixtureWriteSkillBody
	for _, tt := range []struct {
		name       string
		transcript string
	}{
		{"invocation last", noise + fixtureBashSearch},
		{"invocation first", fixtureBashSearch + noise},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want := searchProbe{used: true, source: searchSourceCommand}
			if got := detectSearchUsage(ag, []byte(tt.transcript)); got != want {
				t.Errorf("detectSearchUsage() = %+v, want %+v", got, want)
			}
		})
	}
}

// TestDetectSearchUsage_UnprobeableAgentsReportUnsupported is the test that
// keeps the metric honest, and it is the case the review finding's suggested
// fix would have gotten wrong. These agents are fed a transcript that DOES
// contain a real invocation: reporting used=false with source=none would be a
// fabricated data point indistinguishable in aggregate from a real miss.
//
// Cursor is not merely unimplemented — its transcripts carry no tool_use blocks
// at all, so it can never be probed this way.
func TestDetectSearchUsage_UnprobeableAgentsReportUnsupported(t *testing.T) {
	t.Parallel()

	for _, agentType := range []types.AgentType{agent.AgentTypeCursor, agent.AgentTypePi, agent.AgentTypeCopilotCLI} {
		t.Run(string(agentType), func(t *testing.T) {
			t.Parallel()
			ag, err := agent.GetByAgentType(agentType)
			if err != nil {
				t.Fatalf("GetByAgentType(%s) error: %v", agentType, err)
			}
			want := searchProbe{used: false, source: searchSourceUnsupported}
			if got := detectSearchUsage(ag, []byte(fixtureBashSearch)); got != want {
				t.Errorf("detectSearchUsage(%s) = %+v, want %+v", agentType, got, want)
			}
		})
	}
}

func TestDetectSearchUsage_NilAgentReportsUnsupported(t *testing.T) {
	t.Parallel()
	want := searchProbe{used: false, source: searchSourceUnsupported}
	if got := detectSearchUsage(nil, []byte(fixtureBashSearch)); got != want {
		t.Errorf("detectSearchUsage(nil) = %+v, want %+v", got, want)
	}
}

// TestSearchHintsCoverPattern pins the contract that makes the scanner's byte
// prefilter a performance filter rather than a correctness one: every string
// the matchers accept must contain one of the hints literally. Loosen the
// pattern's internal spacing to \s+ and this fails — which is the point, since
// the failure mode is otherwise a silent false negative.
func TestSearchHintsCoverPattern(t *testing.T) {
	t.Parallel()

	accepted := []string{
		"entire search foo",
		"entire checkpoint search foo",
		"cd sub && entire search foo",
		"ENTIRE_X=1 entire search foo",
		"/usr/local/bin/entire search foo",
		"echo hi; entire search foo",
		"$(entire search foo)",
	}
	for _, cmd := range accepted {
		if !entireSearchCommandPattern.MatchString(cmd) {
			t.Errorf("entireSearchCommandPattern must accept %q", cmd)
			continue
		}
		if !hintedBySearchHints(cmd) {
			t.Errorf("accepted command %q contains no entireSearchHints literal; the scanner would never parse its line", cmd)
		}
	}
	if !hintedBySearchHints(entireSearchSubagent) {
		t.Errorf("subagent name %q contains no entireSearchHints literal", entireSearchSubagent)
	}

	rejected := []string{
		"grep -rn \"entire search\" cmd/",
		"git commit -m \"mention entire search here\"",
		"entire status",
		"entire searchfoo",
	}
	for _, cmd := range rejected {
		if entireSearchCommandPattern.MatchString(cmd) {
			t.Errorf("entireSearchCommandPattern must reject %q", cmd)
		}
	}
}

func hintedBySearchHints(s string) bool {
	for _, hint := range entireSearchHints {
		if bytes.Contains([]byte(s), hint) {
			return true
		}
	}
	return false
}

func TestNewCommitCondensedSignal_CarriesSearchProbe(t *testing.T) {
	t.Parallel()

	probe := searchProbe{used: true, source: searchSourceSubagent}
	sig := newCommitCondensedSignal(
		&SessionState{AgentType: agent.AgentTypeClaudeCode},
		&CondenseResult{SearchProbe: probe, FilesTouched: []string{"a.go"}},
	)
	if sig == nil {
		t.Fatal("newCommitCondensedSignal returned nil")
	}
	if sig.searchProbe != probe {
		t.Errorf("searchProbe = %+v, want %+v", sig.searchProbe, probe)
	}
}

func TestPriorAICommitTouchedFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)

	// Commit 1: an AI checkpoint commit touching ai.txt.
	testutil.WriteFile(t, tmpDir, "ai.txt", "ai content")
	testutil.GitAdd(t, tmpDir, "ai.txt")
	cpID := id.MustCheckpointID("abcdef123456")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("ai change", cpID))

	// Commit 2: a plain human commit touching human.txt.
	testutil.WriteFile(t, tmpDir, "human.txt", "human content")
	testutil.GitAdd(t, tmpDir, "human.txt")
	testutil.GitCommit(t, tmpDir, "human change")

	// Commit 3: HEAD — the commit that was "just created"; --skip=1 must
	// exclude it, so its files never count as prior history.
	testutil.WriteFile(t, tmpDir, "head.txt", "head content")
	testutil.GitAdd(t, tmpDir, "head.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("head change", cpID))

	ctx := t.Context()

	if !priorAICommitTouchedFiles(ctx, tmpDir, []string{"ai.txt"}) {
		t.Error("ai.txt was touched by a prior checkpoint commit; want true")
	}
	if priorAICommitTouchedFiles(ctx, tmpDir, []string{"human.txt"}) {
		t.Error("human.txt was only touched by a human commit; want false")
	}
	if priorAICommitTouchedFiles(ctx, tmpDir, []string{"head.txt"}) {
		t.Error("head.txt was only touched by the just-created HEAD commit; want false")
	}
	if priorAICommitTouchedFiles(ctx, tmpDir, nil) {
		t.Error("no committed files; want false")
	}
	if priorAICommitTouchedFiles(ctx, t.TempDir(), []string{"ai.txt"}) {
		t.Error("not a git repository; want best-effort false")
	}
}

func TestPriorAICommitTouchedFiles_NonASCIIPath(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	testutil.InitRepo(t, tmpDir)

	// Without -z git quotes non-ASCII names in --name-only output
	// ("caf\303\251.go"), which can never match the unquoted FilesTouched
	// form — a systematic false negative this test pins down.
	testutil.WriteFile(t, tmpDir, "café.go", "package main")
	testutil.GitAdd(t, tmpDir, "café.go")
	cpID := id.MustCheckpointID("abcdef123456")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("ai change", cpID))

	// HEAD commit that --skip=1 excludes.
	testutil.WriteFile(t, tmpDir, "head.txt", "head content")
	testutil.GitAdd(t, tmpDir, "head.txt")
	testutil.GitCommit(t, tmpDir, trailers.FormatCheckpoint("head change", cpID))

	if !priorAICommitTouchedFiles(t.Context(), tmpDir, []string{"café.go"}) {
		t.Error("café.go was touched by a prior checkpoint commit; want true")
	}
}
