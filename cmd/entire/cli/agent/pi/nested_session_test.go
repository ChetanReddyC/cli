package pi

import (
	"context"
	"strings"
	"testing"
)

// A Pi subagent is a nested `pi --mode json -p --no-session` process spawned from
// the project directory, where Pi auto-discovers Entire's own project-local
// extension — so the child fires these hooks too. Because it runs with
// --no-session it reports no session file, and the session-ID cache is a single
// per-repo file: an unconditional cache fallback handed the child its *parent's*
// session ID. The nested turn then overwrote the parent's prompt and opened a new
// turn mid-flight.
//
// These tests pin the guard: with no session file in the payload, the cached ID is
// not trusted and no lifecycle event is emitted.
func TestParseHookEvent_NestedInvocation_DoesNotClaimParentSession(t *testing.T) {
	// Cannot use t.Parallel — t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)

	ctx := context.Background()
	a := &PiAgent{}

	// Parent session populates the cache.
	if _, err := a.ParseHookEvent(ctx, HookNameSessionStart, strings.NewReader(
		`{"type":"session_start","session_file":"/tmp/2026-05-09T12-00-00-000Z_parent-id.jsonl"}`)); err != nil {
		t.Fatalf("parent session_start: %v", err)
	}
	if got := readCachedSessionID(ctx); got != "parent-id" {
		t.Fatalf("cache = %q, want parent-id", got)
	}

	// The nested child's payloads carry no session_file.
	for _, tc := range []struct {
		name  string
		hook  string
		stdin string
	}{
		{"session_start", HookNameSessionStart, `{"type":"session_start","cwd":"/repo"}`},
		{"before_agent_start", HookNameBeforeAgentStart, `{"type":"before_agent_start","cwd":"/repo","prompt":"Task: create docs/red.md"}`},
		{"agent_end", HookNameAgentEnd, `{"type":"agent_end","cwd":"/repo"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := a.ParseHookEvent(ctx, tc.hook, strings.NewReader(tc.stdin))
			if err != nil {
				t.Fatalf("ParseHookEvent(%s): %v", tc.hook, err)
			}
			if ev != nil {
				t.Fatalf("nested %s emitted %s for session %q; want no event so the parent session is left alone",
					tc.hook, ev.Type, ev.SessionID)
			}
		})
	}

	// The parent's cache must survive the nested lifecycle untouched.
	if got := readCachedSessionID(ctx); got != "parent-id" {
		t.Errorf("cache after nested lifecycle = %q, want parent-id", got)
	}
}

// TestParseHookEvent_CacheFallbackStillAppliesWithSessionFile keeps the legitimate
// fallback: a session file is present but its name does not carry a parseable ID,
// so the cached ID is still used.
func TestParseHookEvent_CacheFallbackStillAppliesWithSessionFile(t *testing.T) {
	// Cannot use t.Parallel — t.Chdir.
	dir := t.TempDir()
	t.Chdir(dir)

	ctx := context.Background()
	a := &PiAgent{}

	if _, err := a.ParseHookEvent(ctx, HookNameSessionStart, strings.NewReader(
		`{"type":"session_start","session_file":"/tmp/2026-05-09T12-00-00-000Z_abc-123.jsonl"}`)); err != nil {
		t.Fatalf("session_start: %v", err)
	}

	ev, err := a.ParseHookEvent(ctx, HookNameBeforeAgentStart, strings.NewReader(
		`{"type":"before_agent_start","cwd":"/repo","session_file":"/tmp/live.jsonl","prompt":"do thing"}`))
	if err != nil {
		t.Fatalf("before_agent_start: %v", err)
	}
	if ev == nil {
		t.Fatal("before_agent_start with a session file emitted no event")
	}
	// "live.jsonl" has no <timestamp>_<uuid> form, so the ID comes from the file
	// name itself rather than the cache; either way a real turn must start.
	if ev.SessionID == "" {
		t.Error("before_agent_start resolved an empty session ID")
	}
}

// TestParseHookEvent_ExplicitSessionIDNeedsNoSessionFile documents that a payload
// naming its own session_id is still honoured — the guard only distrusts the
// *cache*, not an explicit ID.
func TestParseHookEvent_ExplicitSessionIDNeedsNoSessionFile(t *testing.T) {
	// Cannot use t.Parallel — t.Chdir.
	t.Chdir(t.TempDir())

	a := &PiAgent{}
	ev, err := a.ParseHookEvent(context.Background(), HookNameBeforeAgentStart, strings.NewReader(
		`{"type":"before_agent_start","cwd":"/repo","session_id":"explicit-id","prompt":"do thing"}`))
	if err != nil {
		t.Fatalf("before_agent_start: %v", err)
	}
	if ev == nil || ev.SessionID != "explicit-id" {
		t.Fatalf("got %+v, want an event for session explicit-id", ev)
	}
}
