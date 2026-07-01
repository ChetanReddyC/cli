package checkpoint

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/checkpoint/id"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/transcript/imageextract"
	"github.com/entireio/cli/redact"
)

// claudeTranscriptWithImage returns a Claude Code JSONL transcript whose first
// line embeds an inline base64 image, followed by an ordinary assistant reply.
// It returns the raw (image-inline) bytes plus the base64 string so tests can
// assert on both the extracted and reinjected forms.
func claudeTranscriptWithImage(t *testing.T) (raw []byte, b64 string) {
	t.Helper()
	b64 = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nround-trip-fixture-bytes-long-enough-to-be-externalized\x00\x01\x02\x03"))
	lines := []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":[` +
			`{"type":"text","text":"look at this"},` +
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
			`]}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-01-01T00:00:01Z","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"nice screenshot"}],"usage":{"input_tokens":5,"output_tokens":7}}}`,
	}
	return []byte(strings.Join(lines, "\n") + "\n"), b64
}

// TestAssets_StoreRestoreRoundTrip is the end-to-end contract for image
// externalization at the persistent-store layer: a Claude Code transcript with an
// inline base64 image is externalized before the write, stored as a placeholder
// plus an assets/ blob and manifest, and reinjected byte-exactly on read.
func TestAssets_StoreRestoreRoundTrip(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a55e70000001")

	raw, b64 := claudeTranscriptWithImage(t)

	// Externalize exactly as the condensation path does, then store the
	// placeholder-bearing transcript with its assets.
	codec := imageextract.CodecFor(agent.AgentTypeClaudeCode)
	if codec == nil {
		t.Fatal("expected a Claude Code image codec")
	}
	rewritten, assets, err := codec.ExtractImages(raw)
	if err != nil {
		t.Fatalf("ExtractImages() error = %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 externalized asset, got %d", len(assets))
	}
	writeAssets := make([]TranscriptAsset, len(assets))
	for i, a := range assets {
		writeAssets[i] = TranscriptAsset{Name: a.Name, MediaType: a.MediaType, Data: a.Data}
	}

	if err := store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    "session-assets-001",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(rewritten),
		Assets:       writeAssets,
		Prompts:      []string{"look at this"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"

	// Stored full.jsonl carries the placeholder, not the raw base64.
	stored, ok := readBranchFile(t, store, sessionPath+paths.TranscriptFileName)
	if !ok {
		t.Fatal("full.jsonl missing from checkpoint tree")
	}
	if strings.Contains(stored, b64) {
		t.Error("stored full.jsonl still contains raw base64 image data")
	}
	if !strings.Contains(stored, "entire-asset:assets/"+assets[0].Name) {
		t.Errorf("stored full.jsonl missing placeholder for %s", assets[0].Name)
	}

	// The asset blob and manifest are written under assets/.
	if _, ok := readBranchFile(t, store, sessionPath+paths.AssetsDir+assets[0].Name); !ok {
		t.Errorf("asset blob %s missing from checkpoint tree", assets[0].Name)
	}
	manifest, ok := readBranchFile(t, store, sessionPath+paths.AssetsManifestFile)
	if !ok {
		t.Fatal("assets/manifest.json missing from checkpoint tree")
	}
	if !strings.Contains(manifest, assets[0].Name) || !strings.Contains(manifest, `"media_type": "image/png"`) {
		t.Errorf("manifest missing expected asset entry: %s", manifest)
	}

	// Session metadata points at the manifest.
	summary := readSummaryFromBranch(t, repo, cpID)
	if len(summary.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(summary.Sessions))
	}
	wantManifest := "/" + sessionPath + paths.AssetsManifestFile
	if summary.Sessions[0].AssetsManifest != wantManifest {
		t.Errorf("sessions[0].assets_manifest = %q, want %q", summary.Sessions[0].AssetsManifest, wantManifest)
	}

	// Read back: the image is reinjected byte-exactly, reproducing the original.
	content, err := store.ReadSessionContent(context.Background(), cpID, 0)
	if err != nil {
		t.Fatalf("ReadSessionContent() error = %v", err)
	}
	if strings.Contains(string(content.Transcript), "entire-asset:assets/") {
		t.Error("restored transcript still contains a placeholder")
	}
	if !strings.Contains(string(content.Transcript), b64) {
		t.Error("restored transcript missing reinjected base64 image")
	}
	if string(content.Transcript) != string(raw) {
		t.Fatalf("round-trip not byte-exact:\n got: %s\nwant: %s", content.Transcript, raw)
	}
}

// TestAssets_NoExternalizationWritesNoManifest confirms the default (no assets)
// path is unchanged: no assets/ folder and an empty AssetsManifest pointer.
func TestAssets_NoExternalizationWritesNoManifest(t *testing.T) {
	t.Parallel()
	repo, _ := setupTestRepo(t)
	store := NewGitStore(repo, DefaultV1Refs())
	cpID := id.MustCheckpointID("a55e70000002")

	if err := store.Write(context.Background(), Session{
		CheckpointID: cpID,
		SessionID:    "session-assets-002",
		Strategy:     "manual-commit",
		Transcript:   redact.AlreadyRedacted(claudeStyleTranscript()),
		Prompts:      []string{"hello one"},
		Agent:        agent.AgentTypeClaudeCode,
		AuthorName:   "Test",
		AuthorEmail:  "test@test.com",
	}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	sessionPath := cpID.Path() + "/0/"
	if _, ok := readBranchFile(t, store, sessionPath+paths.AssetsManifestFile); ok {
		t.Error("assets/manifest.json should not be written when there are no assets")
	}
	summary := readSummaryFromBranch(t, repo, cpID)
	if len(summary.Sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(summary.Sessions))
	}
	if summary.Sessions[0].AssetsManifest != "" {
		t.Errorf("sessions[0].assets_manifest = %q, want empty", summary.Sessions[0].AssetsManifest)
	}
}
