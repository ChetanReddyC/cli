package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

const testRolloutPath = "/Users/test/.codex/rollouts/01/01/rollout-20260324-550e8400.jsonl"

// SessionStart and SessionEnd share one parser, so they are covered together.
// SessionEnd (Codex 0.146+) is what finally lets a quit Codex session be
// finalized; its payload is thinner than every other Codex hook — no model, no
// permission_mode, no turn_id — which is why the shared struct must tolerate
// the absent fields.
func TestParseHookEvent_SessionInfoEvents(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		hookName  string
		input     string
		wantType  agent.EventType
		wantModel string
	}{
		{
			name:     "session start",
			hookName: HookNameSessionStart,
			input: `{
				"session_id": "550e8400-e29b-41d4-a716-446655440000",
				"transcript_path": "` + testRolloutPath + `",
				"cwd": "/tmp/testrepo",
				"hook_event_name": "SessionStart",
				"model": "gpt-4.1",
				"permission_mode": "default",
				"source": "startup"
			}`,
			wantType:  agent.SessionStart,
			wantModel: "gpt-4.1",
		},
		{
			name:     "session end",
			hookName: HookNameSessionEnd,
			input: `{
				"session_id": "550e8400-e29b-41d4-a716-446655440000",
				"transcript_path": "` + testRolloutPath + `",
				"cwd": "/tmp/testrepo",
				"hook_event_name": "SessionEnd",
				"reason": "other"
			}`,
			wantType: agent.SessionEnd,
			// Codex omits model on SessionEnd; the session state already has it.
			wantModel: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ag := &CodexAgent{}
			event, err := ag.ParseHookEvent(context.Background(), tt.hookName, strings.NewReader(tt.input))
			require.NoError(t, err)
			require.NotNil(t, event)
			require.Equal(t, tt.wantType, event.Type)
			require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", event.SessionID)
			require.Equal(t, testRolloutPath, event.SessionRef)
			require.Equal(t, "/tmp/testrepo", event.CWD)
			require.Equal(t, tt.wantModel, event.Model)
		})
	}
}

// Ephemeral mode leaves transcript_path null. The session must still parse —
// for SessionEnd that is the whole point of the hook.
func TestParseHookEvent_SessionInfoEventsNullTranscript(t *testing.T) {
	t.Parallel()
	for _, hookName := range []string{HookNameSessionStart, HookNameSessionEnd} {
		t.Run(hookName, func(t *testing.T) {
			t.Parallel()
			ag := &CodexAgent{}
			input := `{
				"session_id": "test-uuid",
				"transcript_path": null,
				"cwd": "/tmp/testrepo"
			}`
			event, err := ag.ParseHookEvent(context.Background(), hookName, strings.NewReader(input))
			require.NoError(t, err)
			require.NotNil(t, event)
			require.Equal(t, "test-uuid", event.SessionID)
			require.Empty(t, event.SessionRef)
		})
	}
}

// Codex enforces SESSION_END_MAX_TIMEOUT_SEC = 3 and kills the hook's process
// tree on expiry, so Entire's self-imposed budget must stay strictly under the
// timeout it configures — with enough headroom to cover the startup that happens
// before Entire can measure anything.
//
// The headroom is not decoration: Codex's cap starts at the `sh -c` wrapper,
// ours at Go package init, and sh startup + the command -v PATH walk + loading a
// ~66MB binary all land in between. Measured ~40ms warm, but a cold page cache or
// a network filesystem is exactly where it is not, and overrunning means the
// process tree is killed mid-condense. Pinned so tightening the budget back up
// has to be a deliberate act.
func TestCodexAgent_SessionEndBudgetFitsConfiguredTimeout(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	budget := ag.SessionEndBudget()
	timeout := time.Duration(SessionEndTimeoutSec) * time.Second

	require.Positive(t, budget)
	require.Less(t, budget, timeout,
		"budget must leave headroom before Codex terminates the hook process tree")
	require.GreaterOrEqual(t, timeout-budget, time.Second,
		"budget must leave at least 1s for wrapper + binary startup, which Codex's clock counts and Entire's does not")
}

func TestParseHookEvent_UserPromptSubmit(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	input := `{
		"session_id": "test-uuid",
		"turn_id": "turn-123",
		"transcript_path": "/tmp/rollout.jsonl",
		"cwd": "/tmp/testrepo",
		"hook_event_name": "UserPromptSubmit",
		"model": "gpt-4.1",
		"permission_mode": "default",
		"prompt": "Create a hello.txt file"
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameUserPromptSubmit, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	require.Equal(t, agent.TurnStart, event.Type)
	require.Equal(t, "test-uuid", event.SessionID)
	require.Equal(t, "/tmp/rollout.jsonl", event.SessionRef)
	require.Equal(t, "Create a hello.txt file", event.Prompt)
	require.Equal(t, "gpt-4.1", event.Model)
}

func TestParseHookEvent_Stop(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	input := `{
		"session_id": "test-uuid",
		"turn_id": "turn-123",
		"transcript_path": "/tmp/rollout.jsonl",
		"cwd": "/tmp/testrepo",
		"hook_event_name": "Stop",
		"model": "gpt-4.1",
		"permission_mode": "default",
		"stop_hook_active": true,
		"last_assistant_message": "Done creating file."
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNameStop, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	require.Equal(t, agent.TurnEnd, event.Type)
	require.Equal(t, "test-uuid", event.SessionID)
	require.Equal(t, "/tmp/rollout.jsonl", event.SessionRef)
	require.Equal(t, "gpt-4.1", event.Model)
}

func TestParseHookEvent_PreToolUse_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// PreToolUse is a pass-through — should return nil event
	event, err := ag.ParseHookEvent(context.Background(), HookNamePreToolUse, strings.NewReader("{}"))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_PostToolUse_ApplyPatch(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// Match the wire shape from codex-rs/hooks/src/schema.rs PostToolUseCommandInput.
	// tool_input.command carries the patch envelope as a single string.
	input := `{
		"session_id": "550e8400-e29b-41d4-a716-446655440000",
		"turn_id": "turn-1",
		"transcript_path": "/tmp/rollout.jsonl",
		"cwd": "/tmp/testrepo",
		"hook_event_name": "PostToolUse",
		"model": "gpt-5",
		"permission_mode": "default",
		"tool_name": "apply_patch",
		"tool_use_id": "call-abc",
		"tool_input": {"command": "*** Begin Patch\n*** Add File: a.txt\n+hi\n*** Update File: b.txt\n@@\n-old\n+new\n*** Delete File: c.txt\n*** End Patch\n"},
		"tool_response": "Success."
	}`

	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	require.NoError(t, err)
	require.NotNil(t, event)
	require.Equal(t, agent.ToolUse, event.Type)
	require.Equal(t, "550e8400-e29b-41d4-a716-446655440000", event.SessionID)
	require.Equal(t, "/tmp/rollout.jsonl", event.SessionRef)
	require.Equal(t, "/tmp/testrepo", event.CWD)
	require.Equal(t, "call-abc", event.ToolUseID)
	require.Equal(t, []string{"a.txt"}, event.NewFiles)
	require.Equal(t, []string{"b.txt"}, event.ModifiedFiles)
	require.Equal(t, []string{"c.txt"}, event.DeletedFiles)
}

func TestParseHookEvent_PostToolUse_AcceptsClaudeAliases(t *testing.T) {
	t.Parallel()
	// Codex registers Write and Edit as matcher aliases for apply_patch
	// (codex-rs/core/src/tools/hook_names.rs). Hook stdin still carries one of
	// those aliases as tool_name when a Claude-style hook config matches by
	// alias, so the parser must accept all three.
	for _, name := range []string{"apply_patch", "Write", "Edit"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ag := &CodexAgent{}
			input := `{
				"session_id": "s",
				"cwd": "/tmp/r",
				"tool_name": "` + name + `",
				"tool_use_id": "id",
				"tool_input": {"command": "*** Begin Patch\n*** Add File: x.txt\n+x\n*** End Patch\n"}
			}`
			event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
			require.NoError(t, err)
			require.NotNil(t, event)
			require.Equal(t, []string{"x.txt"}, event.NewFiles)
		})
	}
}

func TestParseHookEvent_PostToolUse_NonMutatingTool_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// Shell calls fire PostToolUse too, but we can't extract files from them
	// without parsing the shell command. Skip them entirely so we don't churn
	// session state on every command.
	input := `{
		"session_id": "s",
		"cwd": "/tmp/r",
		"tool_name": "shell",
		"tool_use_id": "id",
		"tool_input": {"command": ["echo", "hi"]},
		"tool_response": "hi\n"
	}`
	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_PostToolUse_EmptyPatch_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// A patch envelope with no Add/Update/Delete lines (e.g. malformed input
	// that still parses as JSON) should be a no-op rather than an error.
	input := `{
		"session_id": "s",
		"cwd": "/tmp/r",
		"tool_name": "apply_patch",
		"tool_use_id": "id",
		"tool_input": {"command": "*** Begin Patch\n*** End Patch\n"}
	}`
	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_PostToolUse_MissingToolInput_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	// Defensive: if Codex ever fires PostToolUse for apply_patch with a
	// non-string tool_input shape, we should drop the event rather than fail
	// the hook (which would block the agent's tool call).
	input := `{
		"session_id": "s",
		"cwd": "/tmp/r",
		"tool_name": "apply_patch",
		"tool_use_id": "id",
		"tool_input": null
	}`
	event, err := ag.ParseHookEvent(context.Background(), HookNamePostToolUse, strings.NewReader(input))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_UnknownHook_ReturnsNil(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	event, err := ag.ParseHookEvent(context.Background(), "unknown-hook", strings.NewReader("{}"))
	require.NoError(t, err)
	require.Nil(t, event)
}

func TestParseHookEvent_EmptyInput_ReturnsError(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader(""))
	require.Error(t, err)
}

func TestParseHookEvent_MalformedJSON_ReturnsError(t *testing.T) {
	t.Parallel()
	ag := &CodexAgent{}
	_, err := ag.ParseHookEvent(context.Background(), HookNameSessionStart, strings.NewReader("{invalid json"))
	require.Error(t, err)
}

func TestCodexAgent_ContextInjector(t *testing.T) {
	t.Parallel()
	c := &CodexAgent{}
	require.Equal(t, agent.TurnStart, c.InjectionEvent())
	out, err := c.RenderContextInjection(agent.ContextInjection{Text: "use entire trail"})
	require.NoError(t, err)
	require.Contains(t, string(out), `"hookEventName":"UserPromptSubmit"`)
	require.Contains(t, string(out), `"additionalContext":"use entire trail"`)
	require.True(t, strings.HasSuffix(string(out), "\n"))
}

// testCodexAgentID is the subagent thread id used by the subagent hook tests.
const testCodexAgentID = "child-thread-9"

// TestParseHookEvent_SubagentStart pins the identity mapping, which is the part a
// future reader is most likely to get backwards: Codex's session_id is the identity
// shared by the root thread and all descendants (the user's session), while agent_id
// is the child thread's own id.
func TestParseHookEvent_SubagentStart(t *testing.T) {
	t.Parallel()

	// Field set per subagent-start.command.input.schema.json.
	stdin := strings.NewReader(`{
		"hook_event_name": "SubagentStart",
		"session_id": "root-session-1",
		"agent_id": "child-thread-9",
		"agent_type": "reviewer",
		"transcript_path": "/rollouts/root-session-1.jsonl",
		"cwd": "/repo",
		"model": "gpt-5.4",
		"permission_mode": "default",
		"turn_id": "turn-3"
	}`)

	ev, err := (&CodexAgent{}).ParseHookEvent(context.Background(), HookNameSubagentStart, stdin)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected a SubagentStart event")
	}
	if ev.Type != agent.SubagentStart {
		t.Errorf("Type = %v, want SubagentStart", ev.Type)
	}
	if ev.SessionID != "root-session-1" {
		t.Errorf("SessionID = %q, want the shared root session id", ev.SessionID)
	}
	// Codex sends no tool_use_id; agent_id is the only value that correlates this
	// start with its stop, and Entire keys pre-task state on ToolUseID.
	if ev.ToolUseID != testCodexAgentID {
		t.Errorf("ToolUseID = %q, want the agent id", ev.ToolUseID)
	}
	if ev.SubagentType != "reviewer" {
		t.Errorf("SubagentType = %q, want reviewer", ev.SubagentType)
	}
	if ev.SessionRef != "/rollouts/root-session-1.jsonl" {
		t.Errorf("SessionRef = %q, want the parent rollout", ev.SessionRef)
	}
}

// TestParseHookEvent_SubagentStop covers the two transcripts SubagentStop carries:
// transcript_path is the parent's rollout, agent_transcript_path the subagent's own.
// Declaring the latter is what saves Entire from guessing a layout for Codex.
func TestParseHookEvent_SubagentStop(t *testing.T) {
	t.Parallel()

	stdin := strings.NewReader(`{
		"hook_event_name": "SubagentStop",
		"session_id": "root-session-1",
		"agent_id": "child-thread-9",
		"agent_type": "reviewer",
		"transcript_path": "/rollouts/root-session-1.jsonl",
		"agent_transcript_path": "/rollouts/child-thread-9.jsonl",
		"last_assistant_message": "done",
		"cwd": "/repo",
		"model": "gpt-5.4",
		"permission_mode": "default",
		"stop_hook_active": false,
		"turn_id": "turn-3"
	}`)

	ev, err := (&CodexAgent{}).ParseHookEvent(context.Background(), HookNameSubagentStop, stdin)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected a SubagentEnd event")
	}
	if ev.Type != agent.SubagentEnd {
		t.Errorf("Type = %v, want SubagentEnd", ev.Type)
	}
	if ev.SessionID != "root-session-1" {
		t.Errorf("SessionID = %q, want the shared root session id", ev.SessionID)
	}
	if ev.SubagentID != testCodexAgentID || ev.ToolUseID != testCodexAgentID {
		t.Errorf("SubagentID/ToolUseID = %q/%q, want the agent id for both", ev.SubagentID, ev.ToolUseID)
	}
	if ev.SessionRef != "/rollouts/root-session-1.jsonl" {
		t.Errorf("SessionRef = %q, want the PARENT rollout", ev.SessionRef)
	}
	if ev.SubagentTranscriptPath != "/rollouts/child-thread-9.jsonl" {
		t.Errorf("SubagentTranscriptPath = %q, want the subagent's own rollout", ev.SubagentTranscriptPath)
	}
}

// TestParseHookEvent_SubagentStop_NullTranscripts covers the nullable fields: Codex
// sends null in --ephemeral mode, and a null must not become the string "null".
func TestParseHookEvent_SubagentStop_NullTranscripts(t *testing.T) {
	t.Parallel()

	stdin := strings.NewReader(`{
		"hook_event_name": "SubagentStop",
		"session_id": "root-session-1",
		"agent_id": "child-thread-9",
		"agent_type": "default",
		"transcript_path": null,
		"agent_transcript_path": null,
		"last_assistant_message": null,
		"cwd": "/repo",
		"model": "gpt-5.4",
		"permission_mode": "default",
		"stop_hook_active": false,
		"turn_id": "turn-3"
	}`)

	ev, err := (&CodexAgent{}).ParseHookEvent(context.Background(), HookNameSubagentStop, stdin)
	if err != nil {
		t.Fatalf("ParseHookEvent: %v", err)
	}
	if ev == nil {
		t.Fatal("expected a SubagentEnd event")
	}
	if ev.SessionRef != "" || ev.SubagentTranscriptPath != "" {
		t.Errorf("null transcripts must decode to empty, got SessionRef=%q SubagentTranscriptPath=%q",
			ev.SessionRef, ev.SubagentTranscriptPath)
	}
}
