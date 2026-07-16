package claudecode

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestClaudeCodeAgent_LaunchCmd(t *testing.T) {
	t.Parallel()
	a := NewClaudeCodeAgent()
	launcher, ok := a.(agent.Launcher)
	if !ok {
		t.Fatal("ClaudeCodeAgent does not implement agent.Launcher")
	}
	// Binary may not be on PATH in CI; ErrNotFound is acceptable for this test.
	cmd, err := launcher.LaunchCmd(context.Background(), "hello world")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			t.Skip("claude binary not on PATH; skipping cmd shape check")
		}
		t.Fatalf("LaunchCmd: %v", err)
	}
	if cmd == nil {
		t.Fatal("nil cmd")
	}
	if cmd.Path == "" {
		t.Error("cmd.Path empty")
	}
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "hello world") {
		t.Errorf("args missing prompt: %v", cmd.Args)
	}
}

func TestResolveSessionFile(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{}
	result := ag.ResolveSessionFile("/home/user/.claude/projects/foo", "abc-123-def")
	expected := "/home/user/.claude/projects/foo/abc-123-def.jsonl"
	if result != expected {
		t.Errorf("ResolveSessionFile() = %q, want %q", result, expected)
	}
}

func TestProtectedDirs(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{}
	dirs := ag.ProtectedDirs()
	if len(dirs) != 1 || dirs[0] != ".claude" {
		t.Errorf("ProtectedDirs() = %v, want [.claude]", dirs)
	}
}

func TestGenerateText_LoadsUserSettingsForAuth(t *testing.T) {
	t.Parallel()
	var gotArgs []string
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			gotArgs = args
			return exec.CommandContext(ctx, "sh", "-c", `printf '%s' '{"type":"result","result":"ok"}'`)
		},
	}

	if _, err := ag.GenerateText(context.Background(), "prompt", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	flagValue := func(name string) (string, bool) {
		for i, a := range gotArgs {
			if a == name && i+1 < len(gotArgs) {
				return gotArgs[i+1], true
			}
		}
		return "", false
	}

	// The subprocess must load user settings so API-billing auth (apiKeyHelper /
	// ANTHROPIC_API_KEY approval in ~/.claude/settings.json) is available.
	// Loading no sources ("") made claude report "Not logged in" for those users.
	// See generate.go for the full rationale.
	settingSources, ok := flagValue("--setting-sources")
	if !ok {
		t.Fatalf("--setting-sources flag missing from args: %v", gotArgs)
	}
	if settingSources != settingSourcesUser {
		t.Fatalf("--setting-sources = %q, want %q (empty drops user auth settings)", settingSources, settingSourcesUser)
	}

	// Loading user settings must not let user-level hooks fire on internal
	// text-generation calls, so --settings disables them.
	settings, ok := flagValue("--settings")
	if !ok {
		t.Fatalf("--settings flag missing from args: %v", gotArgs)
	}
	if settings != disableHooksSettings {
		t.Fatalf("--settings = %q, want %q (must disable user hooks)", settings, disableHooksSettings)
	}
}

func TestGenerateText_ArrayResponse(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			response := `[{"type":"system","subtype":"init"},{"type":"assistant","message":"Working on it"},{"type":"result","result":"final generated text"}]`
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+response+"'")
		},
	}

	result, err := ag.GenerateText(context.Background(), "prompt", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "final generated text" {
		t.Fatalf("GenerateText() = %q, want %q", result, "final generated text")
	}
}

func TestGenerateText_EnvelopeErrorReturnsClaudeError(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			response := `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Auth required"}`
			return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+response+"'")
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var ce *ClaudeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v; want *ClaudeError", err)
	}
	if ce.Kind != ClaudeErrorAuth {
		t.Fatalf("Kind = %v; want %v", ce.Kind, ClaudeErrorAuth)
	}
}

func TestGenerateText_CLIMissing(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/nonexistent/binary/that/does/not/exist")
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var ce *ClaudeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v; want *ClaudeError", err)
	}
	if ce.Kind != ClaudeErrorCLIMissing {
		t.Fatalf("Kind = %v; want %v", ce.Kind, ClaudeErrorCLIMissing)
	}
}

func TestGenerateText_StderrAuthFallback(t *testing.T) {
	t.Parallel()
	ag := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "sh", "-c", "printf 'Invalid API key' 1>&2; exit 2")
		},
	}
	_, err := ag.GenerateText(context.Background(), "prompt", "")
	var ce *ClaudeError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v; want *ClaudeError", err)
	}
	if ce.Kind != ClaudeErrorAuth {
		t.Fatalf("Kind = %v; want %v", ce.Kind, ClaudeErrorAuth)
	}
}
