package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestBuildGenerateArgs_IsolatesSettingSources(t *testing.T) {
	t.Parallel()
	// Isolation is the security-critical invariant: --setting-sources must be
	// empty so user-level hooks and tool permissions (e.g. bypassPermissions)
	// are never loaded for this internal, injection-exposed call.
	args := buildGenerateArgs("haiku", "")
	got, ok := flagValue(args, "--setting-sources")
	if !ok {
		t.Fatalf("--setting-sources flag missing from args: %v", args)
	}
	if got != "" {
		t.Fatalf("--setting-sources = %q, want %q (must load no sources)", got, "")
	}
	// With no apiKeyHelper, we inject nothing extra.
	if _, ok := flagValue(args, "--settings"); ok {
		t.Fatalf("--settings must be absent when there is no apiKeyHelper: %v", args)
	}
}

func TestBuildGenerateArgs_InjectsOnlyAPIKeyHelper(t *testing.T) {
	t.Parallel()
	helper := `echo "sk-ant-x" && printf '%s'` // exercises quoting/escaping
	args := buildGenerateArgs("haiku", helper)

	// Sources still empty — we do not fall back to loading the whole file.
	if got, _ := flagValue(args, "--setting-sources"); got != "" {
		t.Fatalf("--setting-sources = %q, want empty", got)
	}

	raw, ok := flagValue(args, "--settings")
	if !ok {
		t.Fatalf("--settings flag missing; apiKeyHelper was not injected: %v", args)
	}
	var injected map[string]any
	if err := json.Unmarshal([]byte(raw), &injected); err != nil {
		t.Fatalf("--settings is not valid JSON: %v (%q)", err, raw)
	}
	if injected["apiKeyHelper"] != helper {
		t.Fatalf("injected apiKeyHelper = %v, want %q", injected["apiKeyHelper"], helper)
	}
	// Must inject ONLY auth — never hooks or permissions.
	if len(injected) != 1 {
		t.Fatalf("--settings must contain only apiKeyHelper, got %v", injected)
	}
}

func TestReadUserAPIKeyHelper_FromClaudeConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"),
		[]byte(`{"apiKeyHelper":"echo secret-cmd","permissions":{"defaultMode":"bypassPermissions"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readUserAPIKeyHelper(); got != "echo secret-cmd" {
		t.Fatalf("readUserAPIKeyHelper() = %q, want %q", got, "echo secret-cmd")
	}
}

func TestReadUserAPIKeyHelper_MissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no settings.json inside
	if got := readUserAPIKeyHelper(); got != "" {
		t.Fatalf("readUserAPIKeyHelper() = %q, want empty for missing file", got)
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
