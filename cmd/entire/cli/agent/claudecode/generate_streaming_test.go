package claudecode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/agent"
)

func TestGenerateTextStreaming_Success(t *testing.T) {
	t.Parallel()

	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_success.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	agentInst := &ClaudeCodeAgent{
		CommandRunner: fakeStreamCmd(string(fixture), "", 0),
	}

	var phases []agent.ProgressPhase
	result, err := agentInst.GenerateTextStreaming(
		context.Background(), "test prompt", "haiku",
		func(p agent.GenerationProgress) {
			phases = append(phases, p.Phase)
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Hello, world." {
		t.Errorf("result = %q, want %q", result, "Hello, world.")
	}
	// We expect Connecting, FirstToken, Generating x2 from the stream,
	// plus a final Done emitted by GenerateTextStreaming itself.
	want := []agent.ProgressPhase{
		agent.PhaseConnecting,
		agent.PhaseFirstToken,
		agent.PhaseGenerating,
		agent.PhaseGenerating,
		agent.PhaseDone,
	}
	if !equalPhases(phases, want) {
		t.Errorf("phases = %v, want %v", phases, want)
	}
}

func TestGenerateTextStreaming_FallbackOnUnrecognizedFlag(t *testing.T) {
	t.Parallel()

	// Old CLI: exit non-zero with stderr complaining about --output-format=stream-json.
	// Fallback path is exercised by routing the *second* call (GenerateText) to a
	// canned non-streaming envelope.
	streamCall := fakeStreamCmd("", "error: unknown flag: --output-format=stream-json", 1)
	nonStreamCall := fakeStreamCmd(`{"is_error":false,"result":"fallback ok","subtype":"success"}`, "", 0)
	calls := 0
	agentInst := &ClaudeCodeAgent{
		CommandRunner: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			calls++
			if calls == 1 {
				return streamCall(ctx, name, args...)
			}
			return nonStreamCall(ctx, name, args...)
		},
	}

	result, err := agentInst.GenerateTextStreaming(context.Background(), "test", "haiku", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "fallback ok" {
		t.Errorf("result = %q, want %q", result, "fallback ok")
	}
	if calls != 2 {
		t.Errorf("expected 2 subprocess invocations (streaming + fallback), got %d", calls)
	}
}

func TestGenerateTextStreaming_EnvelopeErrorSurfaced(t *testing.T) {
	t.Parallel()

	// Verify that an is_error envelope (e.g. HTTP 404) from the result event
	// is surfaced as a typed error containing the API status. The production
	// code checks envelope.IsError BEFORE checking ctx.Err(), so an envelope
	// error wins over context cancellation if both are present — the
	// precedence is verifiable by inspection of generate_streaming.go (the
	// envelope check at the top of the post-Wait branch precedes the
	// ctx.Err() check). This test exercises the envelope-error surfacing
	// path; the precedence ordering itself is not testable here without
	// deterministic timing control over the subprocess lifecycle.
	fixture, err := os.ReadFile(filepath.Join("testdata", "stream_error_404.jsonl"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	agentInst := &ClaudeCodeAgent{
		CommandRunner: fakeStreamCmd(string(fixture), "", 0),
	}
	_, err = agentInst.GenerateTextStreaming(context.Background(), "test", "haiku", nil)
	if err == nil {
		t.Fatal("expected error from is_error envelope")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("expected envelope error, got Canceled")
	}
	// Streaming envelope errors must surface as typed *ClaudeError so the
	// explain layer's formatCheckpointSummaryError can route on Kind
	// (auth/rate-limit/config) instead of substring-matching err.Error().
	var claudeErr *ClaudeError
	if !errors.As(err, &claudeErr) {
		t.Fatalf("expected *ClaudeError, got %T: %v", err, err)
	}
	if claudeErr.APIStatus != 404 {
		t.Errorf("APIStatus = %d, want 404", claudeErr.APIStatus)
	}
	if claudeErr.Kind != ClaudeErrorConfig {
		t.Errorf("Kind = %q, want %q", claudeErr.Kind, ClaudeErrorConfig)
	}
}

func equalPhases(a, b []agent.ProgressPhase) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeStreamCmd returns a CommandRunner factory whose *exec.Cmd, when Start()'d
// and Wait()'d, produces stdout/stderr/exit-code as configured. Implementation
// uses 'sh -c' to write fixture data; on Windows runners we'd need PowerShell,
// but our CI is Linux/macOS.
func fakeStreamCmd(stdout, stderr string, exitCode int) func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		script := buildFakeShellScript(stdout, stderr, exitCode)
		return exec.CommandContext(ctx, "sh", "-c", script)
	}
}

func buildFakeShellScript(stdout, stderr string, exitCode int) string {
	var sb strings.Builder
	if stdout != "" {
		sb.WriteString("cat <<'__EOF__'\n")
		sb.WriteString(stdout)
		if !strings.HasSuffix(stdout, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("__EOF__\n")
	}
	if stderr != "" {
		sb.WriteString("cat <<'__EOF__' 1>&2\n")
		sb.WriteString(stderr)
		if !strings.HasSuffix(stderr, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("__EOF__\n")
	}
	if exitCode != 0 {
		sb.WriteString("exit ")
		sb.WriteString(itoa(exitCode))
		sb.WriteString("\n")
	}
	return sb.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
