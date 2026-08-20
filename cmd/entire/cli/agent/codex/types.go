package codex

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/entireio/cli/cmd/entire/cli/jsonutil"
)

// HooksFile represents the .codex/hooks.json structure.
type HooksFile struct {
	Hooks HookEvents `json:"hooks"`
}

// HookEvents contains the hook configurations by event type.
type HookEvents struct {
	SessionStart     []MatcherGroup `json:"SessionStart,omitempty"`
	SessionEnd       []MatcherGroup `json:"SessionEnd,omitempty"`
	UserPromptSubmit []MatcherGroup `json:"UserPromptSubmit,omitempty"`
	Stop             []MatcherGroup `json:"Stop,omitempty"`
	PreToolUse       []MatcherGroup `json:"PreToolUse,omitempty"`
	PostToolUse      []MatcherGroup `json:"PostToolUse,omitempty"`
	SubagentStart    []MatcherGroup `json:"SubagentStart,omitempty"`
	SubagentStop     []MatcherGroup `json:"SubagentStop,omitempty"`
}

// MatcherGroup groups hooks under an optional matcher pattern.
type MatcherGroup struct {
	Matcher       *string     `json:"matcher"`
	Hooks         []HookEntry `json:"hooks"`
	unknownFields map[string]json.RawMessage
}

// HookEntry represents a single hook command in the config.
type HookEntry struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       int    `json:"timeout,omitempty"`
	unknownFields map[string]json.RawMessage
	hasType       bool
	hasCommand    bool
	hasTimeout    bool
}

// UnmarshalJSON retains fields from newer Codex schemas so hook updates do not
// erase user configuration that this version of Entire does not understand.
func (g *MatcherGroup) UnmarshalJSON(data []byte) error {
	type matcherGroupJSON MatcherGroup
	var decoded matcherGroupJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode matcher group: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode matcher group fields: %w", err)
	}
	delete(fields, "matcher")
	delete(fields, "hooks")
	*g = MatcherGroup(decoded)
	g.unknownFields = fields
	return nil
}

// MarshalJSON restores fields retained by UnmarshalJSON alongside the fields
// Entire understands and may update.
func (g MatcherGroup) MarshalJSON() ([]byte, error) {
	fields := cloneJSONFields(g.unknownFields)
	matcher, err := jsonutil.MarshalWithNoHTMLEscape(g.Matcher)
	if err != nil {
		return nil, fmt.Errorf("encode matcher: %w", err)
	}
	fields["matcher"] = matcher
	hooks, err := jsonutil.MarshalWithNoHTMLEscape(g.Hooks)
	if err != nil {
		return nil, fmt.Errorf("encode matcher hooks: %w", err)
	}
	fields["hooks"] = hooks
	output, err := jsonutil.MarshalWithNoHTMLEscape(fields)
	if err != nil {
		return nil, fmt.Errorf("encode matcher group: %w", err)
	}
	return output, nil
}

// UnmarshalJSON retains fields from hook variants and newer Codex schemas.
func (e *HookEntry) UnmarshalJSON(data []byte) error {
	type hookEntryJSON HookEntry
	var decoded hookEntryJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("decode hook entry: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode hook entry fields: %w", err)
	}
	_, decoded.hasType = fields["type"]
	_, decoded.hasCommand = fields["command"]
	_, decoded.hasTimeout = fields["timeout"]
	delete(fields, "type")
	delete(fields, "command")
	delete(fields, "timeout")
	*e = HookEntry(decoded)
	e.unknownFields = fields
	return nil
}

// MarshalJSON preserves whether optional fields were absent on user-defined
// hook variants while emitting all fields required by new Entire hooks.
func (e HookEntry) MarshalJSON() ([]byte, error) {
	fields := cloneJSONFields(e.unknownFields)
	if e.hasType || e.Type != "" {
		value, err := jsonutil.MarshalWithNoHTMLEscape(e.Type)
		if err != nil {
			return nil, fmt.Errorf("encode hook type: %w", err)
		}
		fields["type"] = value
	}
	if e.hasCommand || e.Command != "" {
		value, err := jsonutil.MarshalWithNoHTMLEscape(e.Command)
		if err != nil {
			return nil, fmt.Errorf("encode hook command: %w", err)
		}
		fields["command"] = value
	}
	if e.hasTimeout || e.Timeout != 0 {
		fields["timeout"] = json.RawMessage(strconv.Itoa(e.Timeout))
	}
	output, err := jsonutil.MarshalWithNoHTMLEscape(fields)
	if err != nil {
		return nil, fmt.Errorf("encode hook entry: %w", err)
	}
	return output, nil
}

func cloneJSONFields(fields map[string]json.RawMessage) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(fields)+3)
	for key, value := range fields {
		cloned[key] = value
	}
	return cloned
}

// sessionInfoRaw is the JSON structure shared by the session-scoped hooks,
// SessionStart and SessionEnd, which differ only in the event they represent.
//
// SessionEnd's payload is a strict subset: it carries no model or
// permission_mode (it fires after teardown, not within a turn), and swaps
// `source` for a `reason` that is the constant "other" in Codex today, so it
// cannot distinguish quit from /clear. Unmarshalling is not strict, so the
// absent fields simply stay zero.
type sessionInfoRaw struct {
	SessionID      string  `json:"session_id"`
	TranscriptPath *string `json:"transcript_path"` // nullable (ephemeral mode)
	CWD            string  `json:"cwd"`
	HookEventName  string  `json:"hook_event_name"`
	Model          string  `json:"model"`
	PermissionMode string  `json:"permission_mode"`
	Source         string  `json:"source"` // SessionStart: "startup", "resume", "clear"
}

// userPromptSubmitRaw is the JSON structure from UserPromptSubmit hooks.
type userPromptSubmitRaw struct {
	SessionID      string  `json:"session_id"`
	TurnID         string  `json:"turn_id"`
	TranscriptPath *string `json:"transcript_path"` // nullable
	CWD            string  `json:"cwd"`
	HookEventName  string  `json:"hook_event_name"`
	Model          string  `json:"model"`
	PermissionMode string  `json:"permission_mode"`
	Prompt         string  `json:"prompt"`
}

// postToolUseRaw is the JSON structure from PostToolUse hooks.
// Schema source: codex-rs/hooks/src/schema.rs PostToolUseCommandInput.
// We only consume the fields we need; unknown fields are ignored.
type postToolUseRaw struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath *string         `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	Model          string          `json:"model"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// applyPatchToolInput is the tool_input shape for apply_patch.
// Codex serializes the patch envelope as a single string under "command".
type applyPatchToolInput struct {
	Command string `json:"command"`
}

// stopRaw is the JSON structure from Stop hooks.
type stopRaw struct {
	SessionID            string  `json:"session_id"`
	TurnID               string  `json:"turn_id"`
	TranscriptPath       *string `json:"transcript_path"` // nullable
	CWD                  string  `json:"cwd"`
	HookEventName        string  `json:"hook_event_name"`
	Model                string  `json:"model"`
	PermissionMode       string  `json:"permission_mode"`
	StopHookActive       bool    `json:"stop_hook_active"`
	LastAssistantMessage *string `json:"last_assistant_message"` // nullable
}

// derefString safely dereferences a nullable string pointer.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// subagentStartRaw is Codex's SubagentStart payload
// (codex-rs/hooks/schema/generated/subagent-start.command.input.schema.json).
//
// session_id is the identity shared by the root thread and all descendants — the
// user's session — while agent_id is this subagent thread's own id.
type subagentStartRaw struct {
	SessionID      string  `json:"session_id"`
	AgentID        string  `json:"agent_id"`
	AgentType      string  `json:"agent_type"`
	TranscriptPath *string `json:"transcript_path"` // nullable; parent rollout
	CWD            string  `json:"cwd"`
	HookEventName  string  `json:"hook_event_name"`
	Model          string  `json:"model"`
	PermissionMode string  `json:"permission_mode"`
	TurnID         string  `json:"turn_id"`
}

// subagentStopRaw is Codex's SubagentStop payload
// (codex-rs/hooks/schema/generated/subagent-stop.command.input.schema.json).
//
// Note the two transcripts: transcript_path is the parent thread's rollout,
// agent_transcript_path is the subagent's own.
type subagentStopRaw struct {
	SessionID            string  `json:"session_id"`
	AgentID              string  `json:"agent_id"`
	AgentType            string  `json:"agent_type"`
	TranscriptPath       *string `json:"transcript_path"`       // nullable; parent rollout
	AgentTranscriptPath  *string `json:"agent_transcript_path"` // nullable; subagent rollout
	LastAssistantMessage *string `json:"last_assistant_message"`
	CWD                  string  `json:"cwd"`
	HookEventName        string  `json:"hook_event_name"`
	Model                string  `json:"model"`
	PermissionMode       string  `json:"permission_mode"`
	StopHookActive       bool    `json:"stop_hook_active"`
	TurnID               string  `json:"turn_id"`
}
