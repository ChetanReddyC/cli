package strategy

import (
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/entireio/cli/cmd/entire/cli/trailers"
)

func TestTranscriptMentionsEntireSearch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		transcript string
		want       bool
	}{
		{"canonical form", `{"tool":"Bash","command":"entire search \"retry backoff\" --json"}`, true},
		{"legacy checkpoint form", `{"tool":"Bash","command":"entire checkpoint search foo"}`, true},
		{"no search", `{"tool":"Bash","command":"entire status"}`, false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := transcriptMentionsEntireSearch([]byte(tt.transcript)); got != tt.want {
				t.Errorf("transcriptMentionsEntireSearch(%q) = %v, want %v", tt.transcript, got, tt.want)
			}
		})
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
