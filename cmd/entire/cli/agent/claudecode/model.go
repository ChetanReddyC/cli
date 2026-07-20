package claudecode

import (
	"bytes"
	"encoding/json"
)

// modelScanLine captures the two places a Claude Code transcript records the
// model: the top-level "model" on the system/init line, and "message.model" on
// each assistant message. Subtype distinguishes the init envelope from other
// "system" envelopes.
type modelScanLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Model   string `json:"model"`
	Message struct {
		Model string `json:"model"`
	} `json:"message"`
}

// ExtractModel returns the model identifier from a Claude Code transcript.
//
// Claude Code only reports the model on the SessionStart hook payload, so when
// SessionStart never fired for a session (hooks installed mid-session, a
// resumed/continued session, or a cleared model hint) the model is otherwise
// unknown and checkpoints fall back to "Unknown" attribution. The transcript,
// however, always records it: on the system/init line ("model") and on every
// assistant message ("message.model"). This lets condensation backfill the
// model from the transcript, matching Pi/Copilot/Factory Droid.
//
// The most recent assistant "message.model" wins (reflecting a mid-session
// model switch, and matching the clean, hook-consistent identifier). The
// system/init "model" is a fallback for very short transcripts that have no
// assistant message yet. Returns "" when neither source carries a model.
func (c *ClaudeCodeAgent) ExtractModel(transcriptData []byte) (string, error) {
	var assistantModel, initModel string
	for raw := range bytes.SplitSeq(transcriptData, []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var line modelScanLine
		if err := json.Unmarshal(raw, &line); err != nil {
			// Skip malformed lines (e.g. an incompletely-flushed transcript)
			// rather than aborting the whole scan.
			continue
		}
		switch line.Type {
		case envelopeTypeAssistant:
			if line.Message.Model != "" {
				assistantModel = line.Message.Model
			}
		case "system":
			if line.Subtype == "init" && initModel == "" && line.Model != "" {
				initModel = line.Model
			}
		}
	}
	if assistantModel != "" {
		return assistantModel, nil
	}
	return initModel, nil
}
