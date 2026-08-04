//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/gitrepo"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// codexCiphertext stands in for a Codex encrypted reasoning payload: long enough
// to be unmistakable in a stored blob, and shaped like the real base64.
var codexCiphertext = strings.Repeat("QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVph", 40)

// codexRolloutWithEncryptedReasoning builds a Codex rollout whose reasoning items
// carry encrypted_content, plus a compaction item. Both are non-portable state that
// Entire must strip from its own stored copy (see codex.SanitizePortableTranscript).
func codexRolloutWithEncryptedReasoning(sessionID, repoDir, ciphertext string) string {
	return strings.Join([]string{
		`{"timestamp":"2026-01-01T00:00:00Z","type":"session_meta","payload":{"id":"` + sessionID + `","cwd":"` + repoDir + `"}}`,
		`{"timestamp":"2026-01-01T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"add feature.txt"}]}}`,
		`{"timestamp":"2026-01-01T00:00:02Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"` + ciphertext + `"}}`,
		`{"timestamp":"2026-01-01T00:00:03Z","type":"response_item","payload":{"type":"compaction","payload":{"blob":"` + ciphertext + `"}}}`,
		`{"timestamp":"2026-01-01T00:00:04Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"added feature.txt"}]}}`,
	}, "\n") + "\n"
}

// findShadowSessionTranscript locates the session transcript blob inside a shadow
// branch tree. It searches rather than reconstructing the path because the metadata
// directory is named after the date-prefixed Entire session ID, not the agent's
// raw session_id.
func findShadowSessionTranscript(t *testing.T, repoDir, branchName string) (string, bool) {
	t.Helper()

	repo, err := gitrepo.OpenPath(repoDir)
	if err != nil {
		t.Fatalf("open repo: %v", err)
	}
	defer repo.Close()

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branchName), true)
	if err != nil {
		return "", false
	}
	commit, err := repo.CommitObject(ref.Hash())
	if err != nil {
		return "", false
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", false
	}

	var content string
	var found bool
	err = tree.Files().ForEach(func(f *object.File) error {
		if found {
			return nil
		}
		if !strings.HasPrefix(f.Name, paths.EntireMetadataDir+"/") {
			return nil
		}
		if !strings.HasSuffix(f.Name, "/"+paths.TranscriptFileName) {
			return nil
		}
		c, cErr := f.Contents()
		if cErr != nil {
			return cErr
		}
		content = c
		found = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk shadow tree: %v", err)
	}
	return content, found
}

// codexHooker returns a helper that drives the real Codex hook binary.
func codexHooker(t *testing.T, repoDir, sessionID, transcriptPath string) func(string, map[string]any) {
	t.Helper()
	runner := NewCodexHookRunner(repoDir, t)
	return func(name string, extra map[string]any) {
		t.Helper()
		in := map[string]any{
			"session_id":      sessionID,
			"transcript_path": transcriptPath,
			"cwd":             repoDir,
			"model":           "gpt-5",
			"permission_mode": "default",
		}
		for k, v := range extra {
			in[k] = v
		}
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal %s input: %v", name, err)
		}
		if err := runner.runCodexHook(name, b); err != nil {
			t.Fatalf("codex hook %s: %v", name, err)
		}
	}
}

func applyPatchHook(hook func(string, map[string]any), toolUseID, patch string) {
	hook("post-tool-use", map[string]any{
		"hook_event_name": "PostToolUse", "tool_name": "apply_patch",
		"tool_use_id":   toolUseID,
		"tool_input":    map[string]string{"command": patch},
		"tool_response": "Success.",
	})
}

// TestCodexShadowBranch_SanitizesTranscript proves that the shadow-branch copy of a
// Codex rollout has the non-portable payloads stripped.
//
// Before the fix, lifecycle wrote the raw rollout to .entire/metadata/<session>/full.jsonl
// and the generic metadata-dir walker (addDirectoryToChanges -> createRedactedBlobFromFile)
// redacted every blob without ever sanitizing — so encrypted_content ciphertext landed in
// the shadow tree, and the 8 redaction layers had to scan all of it first (base64 is the
// pathological input for the entropy layer).
func TestCodexShadowBranch_SanitizesTranscript(t *testing.T) {
	env := NewFeatureBranchEnv(t)

	// Long enough to be unmistakable in the blob, and shaped like the real thing.
	ciphertext := codexCiphertext

	sessionID := "codex-shadow-sanitize"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollout.jsonl")

	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	rollout := codexRolloutWithEncryptedReasoning(sessionID, env.RepoDir, ciphertext)
	if err := os.WriteFile(transcriptPath, []byte(rollout), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	hook := codexHooker(t, env.RepoDir, sessionID, transcriptPath)

	// Turn start, then a file-mutating tool use so SaveStep writes a shadow checkpoint.
	// The file must exist on disk (uncommitted) when Stop fires, so the ephemeral
	// write has worktree changes to snapshot.
	hook("user-prompt-submit", map[string]any{
		"prompt": "add feature.txt", "hook_event_name": "UserPromptSubmit",
	})
	env.WriteFile("feature.txt", "hi\n")
	applyPatchHook(hook, "call_1", "*** Begin Patch\n*** Add File: feature.txt\n+hi\n*** End Patch\n")
	hook("stop", map[string]any{"hook_event_name": "Stop"})

	shadowBranch := env.GetShadowBranchName()
	if !env.BranchExists(shadowBranch) {
		t.Fatalf("shadow branch %s should exist after Codex stop", shadowBranch)
	}

	stored, ok := findShadowSessionTranscript(t, env.RepoDir, shadowBranch)
	if !ok {
		t.Fatalf("shadow branch %s has no session transcript", shadowBranch)
	}

	if strings.Contains(stored, ciphertext) {
		t.Error("shadow-branch transcript still contains encrypted_content ciphertext (not sanitized)")
	}
	if strings.Contains(stored, `"encrypted_content"`) {
		t.Error("shadow-branch transcript still has an encrypted_content key")
	}
	if strings.Contains(stored, `"compaction"`) {
		t.Error("shadow-branch transcript still has a compaction item")
	}

	// Sanitization must not eat the actual conversation.
	if !strings.Contains(stored, "add feature.txt") {
		t.Error("shadow-branch transcript lost the user prompt")
	}
	if !strings.Contains(stored, "added feature.txt") {
		t.Error("shadow-branch transcript lost the assistant reply")
	}
}

// TestCodexShadowBranch_GrowthStillDetectedAfterCommit is the regression guard for the
// coordinate coupling that sanitization introduces.
//
// sessionHasNewContent compares the shadow transcript blob's size against
// state.CheckpointTranscriptSize, the baseline recorded at the previous condensation.
// Sanitizing the shadow blob shrinks it by ~99% for Codex, so if the baseline keeps
// being measured on the raw transcript, `transcriptBlobSize > CheckpointTranscriptSize`
// is false forever and the session never condenses again after its first commit.
func TestCodexShadowBranch_GrowthStillDetectedAfterCommit(t *testing.T) {
	env := NewFeatureBranchEnv(t)

	ciphertext := codexCiphertext
	sessionID := "codex-shadow-growth"
	transcriptPath := filepath.Join(env.RepoDir, ".entire", "tmp", "codex-rollout.jsonl")

	if err := os.MkdirAll(filepath.Dir(transcriptPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeRollout := func(content string) {
		t.Helper()
		if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
			t.Fatalf("write rollout: %v", err)
		}
	}

	hook := codexHooker(t, env.RepoDir, sessionID, transcriptPath)

	// Turn 1: work, then commit — this condenses and records the growth baseline.
	writeRollout(codexRolloutWithEncryptedReasoning(sessionID, env.RepoDir, ciphertext))
	hook("user-prompt-submit", map[string]any{
		"prompt": "add feature.txt", "hook_event_name": "UserPromptSubmit",
	})
	env.WriteFile("feature.txt", "hi\n")
	applyPatchHook(hook, "call_1", "*** Begin Patch\n*** Add File: feature.txt\n+hi\n*** End Patch\n")
	hook("stop", map[string]any{"hook_event_name": "Stop"})

	env.GitCommitWithShadowHooks("add feature.txt", "feature.txt")

	firstCheckpoint := env.GetLatestCheckpointIDFromHistory()
	if firstCheckpoint == "" {
		t.Fatal("first commit produced no checkpoint")
	}

	// Turn 2: the rollout grows with a genuinely new exchange, then commit again.
	grown := codexRolloutWithEncryptedReasoning(sessionID, env.RepoDir, ciphertext) +
		`{"timestamp":"2026-01-01T00:01:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"now add second.txt"}]}}` + "\n" +
		`{"timestamp":"2026-01-01T00:01:01Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"` + ciphertext + `"}}` + "\n" +
		`{"timestamp":"2026-01-01T00:01:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"added second.txt"}]}}` + "\n"
	writeRollout(grown)

	hook("user-prompt-submit", map[string]any{
		"prompt": "now add second.txt", "hook_event_name": "UserPromptSubmit",
	})
	env.WriteFile("second.txt", "yo\n")
	applyPatchHook(hook, "call_2", "*** Begin Patch\n*** Add File: second.txt\n+yo\n*** End Patch\n")
	hook("stop", map[string]any{"hook_event_name": "Stop"})

	env.GitCommitWithShadowHooks("add second.txt", "second.txt")

	secondCheckpoint := env.GetLatestCheckpointIDFromHistory()
	if secondCheckpoint == "" {
		t.Fatal("second commit produced no checkpoint")
	}
	if secondCheckpoint == firstCheckpoint {
		t.Fatalf("second commit did not condense a new checkpoint (growth went undetected); "+
			"both commits report checkpoint %s", firstCheckpoint)
	}
}
