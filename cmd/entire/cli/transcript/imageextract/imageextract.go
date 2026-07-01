// Package imageextract externalizes inline base64 images from an agent's session
// transcript into a checkpoint asset store, replacing each with a compact,
// path-bearing placeholder, and re-injects them byte-exactly on restore.
//
// The transform is per-agent because transcript formats differ: only agents that
// inline base64 images (Claude Code today) register a codec; every other agent
// resolves to nil and its transcript flows through untouched (a graceful no-op).
//
// Correctness contract: for any transcript x from a supported agent,
// ReinjectImages(ExtractImages(x)) == x, byte-for-byte. This is achieved by only
// ever swapping the base64 image *value* in place (never re-marshalling the JSON)
// and by refusing to externalize any image whose raw bytes don't re-encode to the
// exact original base64 string.
package imageextract

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Asset is one externalized image. It reuses the canonical agent asset model;
// Name is the stable asset filename (also the id used in the placeholder).
type Asset = agent.CompactedTranscriptAsset

// ImageCodec extracts/reinjects inline images for one agent's transcript format.
type ImageCodec interface {
	// ExtractImages lifts inline base64 images out, returning the rewritten
	// (placeholder-bearing) transcript plus the extracted assets. A transcript
	// with no externalizable images is returned unchanged with nil assets.
	ExtractImages(transcript []byte) (rewritten []byte, assets []Asset, err error)
	// ReinjectImages restores the original transcript by looking each placeholder's
	// asset up by Name. Placeholders whose asset is missing are left in place.
	ReinjectImages(transcript []byte, lookup func(name string) (Asset, bool)) ([]byte, error)
}

// placeholderPrefix leads every externalized-image reference. It is deliberately
// low-entropy so the (later) redaction pass never flags it, and it carries the
// asset's path so an agent summarizing the stored log still understands an image
// was here and where it lives.
const placeholderPrefix = "entire-asset:assets/"

// placeholderRe matches a full placeholder and captures the asset name.
var placeholderRe = regexp.MustCompile(`entire-asset:assets/(img-[0-9a-f]+\.[a-z0-9]+)`)

// newAssetID returns a random hex id (16 bytes → 32 hex chars). Hex is ~4
// bits/char, below the redaction entropy threshold, so placeholders survive
// redaction. Injectable for deterministic tests.
var newAssetID = func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// minExternalizedBase64Len is the shortest base64 image value we externalize.
// It must exceed the 32-char random-hex run in a placeholder so that an
// externalized value can never be a substring of any placeholder — that is the
// single condition under which the in-place value swap could corrupt a
// placeholder and break the byte-exact round trip. As a bonus it leaves tiny
// blobs (which are never real images) inline, where they cost nothing.
const minExternalizedBase64Len = 64

var codecs = map[types.AgentType]ImageCodec{
	agent.AgentTypeClaudeCode: claudeCodec{},
}

// CodecFor returns the image codec for an agent type, or nil if the agent's
// transcript is not known to inline images (a no-op).
func CodecFor(t types.AgentType) ImageCodec { return codecs[t] }

// HasPlaceholders reports whether a transcript carries any externalized-image
// placeholders — so restore knows to reinject regardless of the current config.
func HasPlaceholders(transcript []byte) bool {
	return bytes.Contains(transcript, []byte(placeholderPrefix))
}

// claudeCodec handles Claude Code (and, structurally, Cursor) JSONL transcripts,
// which embed images as {"type":"image","source":{"type":"base64","media_type":…,"data":…}}.
type claudeCodec struct{}

type imgHit struct{ data, mediaType string }

func (claudeCodec) ExtractImages(transcript []byte) ([]byte, []Asset, error) {
	if len(transcript) == 0 {
		return transcript, nil, nil
	}

	// Map each unique base64 image value → its asset (dedupes repeats within the
	// transcript; git dedupes identical blobs across checkpoints by content).
	seen := map[string]Asset{}
	var order []string // unique base64 values, later sorted longest-first

	for _, line := range bytes.Split(transcript, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var v any
		if err := json.Unmarshal(trimmed, &v); err != nil {
			continue // non-JSON line; leave untouched
		}
		var hits []imgHit
		collectBase64Images(v, &hits)
		for _, h := range hits {
			if len(h.data) < minExternalizedBase64Len {
				continue // too small to be a real image; also keeps it out of placeholders
			}
			if _, ok := seen[h.data]; ok {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(h.data)
			if err != nil {
				continue // not standard base64; leave inline
			}
			// Only externalize if re-encoding reproduces the exact original string;
			// otherwise the restore round-trip could not be byte-exact.
			if base64.StdEncoding.EncodeToString(raw) != h.data {
				continue
			}
			name := "img-" + newAssetID() + "." + extForMedia(h.mediaType)
			seen[h.data] = Asset{Name: name, MediaType: h.mediaType, Data: raw}
			order = append(order, h.data)
		}
	}

	if len(order) == 0 {
		return transcript, nil, nil
	}

	// Replace longest values first so that if one image's base64 is a substring
	// of another's, the containing (longer) value is swapped out before the
	// shorter one, which keeps every asset's placeholder present and the swap
	// byte-exact. Ties broken by value for determinism.
	sort.SliceStable(order, func(i, j int) bool {
		if len(order[i]) != len(order[j]) {
			return len(order[i]) > len(order[j])
		}
		return order[i] < order[j]
	})

	rewritten := transcript
	assets := make([]Asset, 0, len(order))
	for _, data := range order {
		a := seen[data]
		rewritten = bytes.ReplaceAll(rewritten, []byte(data), []byte(placeholderPrefix+a.Name))
		assets = append(assets, a)
	}
	return rewritten, assets, nil
}

func (claudeCodec) ReinjectImages(transcript []byte, lookup func(name string) (Asset, bool)) ([]byte, error) {
	if !HasPlaceholders(transcript) {
		return transcript, nil
	}
	result := transcript
	done := map[string]bool{}
	for _, m := range placeholderRe.FindAllSubmatch(transcript, -1) {
		full, name := m[0], string(m[1])
		if done[name] {
			continue
		}
		done[name] = true
		a, ok := lookup(name)
		if !ok {
			continue // asset unavailable; leave the placeholder (best-effort)
		}
		result = bytes.ReplaceAll(result, full, []byte(base64.StdEncoding.EncodeToString(a.Data)))
	}
	return result, nil
}

// collectBase64Images walks any decoded JSON value and gathers every inline
// base64 image block, at any nesting depth (top-level content, tool_result
// content, etc.).
func collectBase64Images(v any, out *[]imgHit) {
	switch t := v.(type) {
	case map[string]any:
		if t["type"] == "image" {
			if src, ok := t["source"].(map[string]any); ok && src["type"] == "base64" {
				if data, ok := src["data"].(string); ok && data != "" {
					var mediaType string
					if mt, mok := src["media_type"].(string); mok {
						mediaType = mt
					}
					*out = append(*out, imgHit{data: data, mediaType: mediaType})
				}
			}
		}
		for _, vv := range t {
			collectBase64Images(vv, out)
		}
	case []any:
		for _, vv := range t {
			collectBase64Images(vv, out)
		}
	}
}

func extForMedia(mediaType string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return "bin"
	}
}
