package imageextract

import (
	"encoding/base64"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

func lookupFrom(assets []Asset) func(string) (Asset, bool) {
	return func(name string) (Asset, bool) {
		for _, a := range assets {
			if a.Name == name {
				return a, true
			}
		}
		return Asset{}, false
	}
}

func claudeLine(b64 string) string {
	return `{"type":"user","message":{"role":"user","content":[` +
		`{"type":"text","text":"look at this"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + b64 + `"}}` +
		`]}}`
}

// The core contract: extract then reinject reproduces the original bytes exactly.
func TestClaudeCodec_RoundTripByteExact(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	if c == nil {
		t.Fatal("expected a codec for Claude Code")
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake-png-bytes-with-enough-length-to-be-a-real-image\x00\x01\x02"))
	orig := claudeLine(b64) + "\n{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n"

	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if strings.Contains(string(rewritten), b64) {
		t.Error("base64 must be gone from the rewritten transcript")
	}
	if !strings.Contains(string(rewritten), placeholderPrefix) {
		t.Error("rewritten transcript should carry a placeholder")
	}
	if assets[0].MediaType != "image/png" {
		t.Errorf("asset media type = %q, want image/png", assets[0].MediaType)
	}

	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatalf("round-trip not byte-exact:\n got: %s\nwant: %s", restored, orig)
	}
}

// A transcript with no images is returned unchanged with no assets.
func TestClaudeCodec_NoImagesIsNoOp(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	orig := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if assets != nil {
		t.Errorf("expected no assets, got %d", len(assets))
	}
	if string(rewritten) != orig {
		t.Errorf("no-image transcript should be unchanged")
	}
}

// Identical images dedupe to one asset but round-trip both occurrences.
func TestClaudeCodec_DedupesIdenticalImages(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	b64 := base64.StdEncoding.EncodeToString([]byte("same-image-bytes-repeated-with-enough-length-to-externalize"))
	orig := claudeLine(b64) + "\n" + claudeLine(b64) + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("identical images should dedupe to 1 asset, got %d", len(assets))
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatalf("round-trip mismatch for duplicated image")
	}
}

// When one image's base64 is a substring of another's, the round trip must still
// be byte-exact (longest-first replacement guarantees this).
func TestClaudeCodec_SubstringImagesRoundTrip(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	long := base64.StdEncoding.EncodeToString([]byte(
		"prefix-bytes-AAAABBBBCCCCDDDD-and-a-considerably-longer-image-tail-payload-so-a-64-char-substring-fits-xyz"))
	// A canonical base64 substring of long (>= threshold) that decodes/re-encodes
	// cleanly, taken from a non-zero offset so it is genuinely embedded.
	var short string
	for i := 4; i+64 <= len(long); i += 4 {
		cand := long[i : i+64]
		if raw, err := base64.StdEncoding.DecodeString(cand); err == nil && base64.StdEncoding.EncodeToString(raw) == cand {
			short = cand
			break
		}
	}
	if short == "" {
		t.Fatal("could not construct a canonical base64 substring")
	}
	// Shorter block first, so first-seen order would (without the sort) replace it
	// before the containing longer value.
	orig := claudeLine(short) + "\n" + claudeLine(long) + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("expected 2 assets, got %d", len(assets))
	}
	// Longest-first replacement means both assets have a live placeholder (neither
	// is orphaned by the other's swap).
	for _, a := range assets {
		if !strings.Contains(string(rewritten), placeholderPrefix+a.Name) {
			t.Errorf("asset %s has no placeholder in the rewritten transcript (orphaned)", a.Name)
		}
	}
	restored, err := c.ReinjectImages(rewritten, lookupFrom(assets))
	if err != nil {
		t.Fatalf("ReinjectImages: %v", err)
	}
	if string(restored) != orig {
		t.Fatalf("substring round-trip not byte-exact:\n got: %s\nwant: %s", restored, orig)
	}
}

// Base64 values too short to be a real image are left inline (and can therefore
// never collide with a placeholder's hex id).
func TestClaudeCodec_LeavesTinyBase64Inline(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	tiny := base64.StdEncoding.EncodeToString([]byte("tiny-blob")) // < minExternalizedBase64Len
	if len(tiny) >= minExternalizedBase64Len {
		t.Fatalf("test fixture too long: %d", len(tiny))
	}
	orig := claudeLine(tiny) + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 0 || string(rewritten) != orig {
		t.Errorf("tiny base64 must be left inline; assets=%d changed=%v", len(assets), string(rewritten) != orig)
	}
}

// Non-base64 image sources (e.g. url) and non-decodable data are left inline.
func TestClaudeCodec_LeavesNonBase64Inline(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	orig := `{"type":"user","message":{"content":[{"type":"image","source":{"type":"url","url":"https://x/y.png"}}]}}` + "\n"
	rewritten, assets, err := c.ExtractImages([]byte(orig))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	if len(assets) != 0 || string(rewritten) != orig {
		t.Errorf("url image source must be left inline; assets=%d changed=%v", len(assets), string(rewritten) != orig)
	}
}

// Agents that don't inline images have no codec (graceful no-op upstream).
func TestCodecFor_NonImageAgentsAreNil(t *testing.T) {
	t.Parallel()
	for _, at := range []string{"Codex", "Gemini CLI", "OpenCode", "Pi", "Factory AI Droid", "Copilot CLI"} {
		if CodecFor(types.AgentType(at)) != nil {
			t.Errorf("agent %q should not have an image codec yet", at)
		}
	}
}

// The placeholder must stay low-entropy so the downstream redaction pass never
// flags it. Redaction's entropy detector runs over each [A-Za-z0-9+_=-]{10,}
// RUN (threshold 4.5 bits/char), not the whole string, so mirror that here.
func TestPlaceholder_RunsAreLowEntropy(t *testing.T) {
	t.Parallel()
	c := CodecFor(agent.AgentTypeClaudeCode)
	b64 := base64.StdEncoding.EncodeToString([]byte("entropy-check-bytes-xyz-padded-to-exceed-the-externalize-threshold"))
	rewritten, _, err := c.ExtractImages([]byte(claudeLine(b64) + "\n"))
	if err != nil {
		t.Fatalf("ExtractImages: %v", err)
	}
	ph := placeholderRe.Find(rewritten)
	if ph == nil {
		t.Fatal("no placeholder produced")
	}
	runRe := regexp.MustCompile(`[A-Za-z0-9+_=-]{10,}`)
	runs := runRe.FindAll(ph, -1)
	if len(runs) == 0 {
		t.Fatalf("expected at least one detector-sized run in %s", ph)
	}
	for _, run := range runs {
		if e := shannonBitsPerChar(run); e >= 4.5 {
			t.Errorf("placeholder run %q entropy %.2f >= 4.5 — redaction could flag it", run, e)
		}
	}
}

func shannonBitsPerChar(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var counts [256]int
	for _, c := range b {
		counts[c]++
	}
	var e float64
	n := float64(len(b))
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		e -= p * math.Log2(p)
	}
	return e
}
