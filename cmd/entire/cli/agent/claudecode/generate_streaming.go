package claudecode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/logging"
)

// GenerateTextStreaming runs the Claude CLI in stream-json mode, dispatches
// progress events to the optional callback, and returns the final result text.
// Implements the agent.StreamingTextGenerator interface.
//
// If the CLI rejects the stream-json flags (older Claude CLI), this falls back
// to the non-streaming GenerateText path — without progress events.
func (c *ClaudeCodeAgent) GenerateTextStreaming(
	ctx context.Context,
	prompt, model string,
	progress agent.ProgressFn,
) (string, error) {
	if model == "" {
		model = "haiku"
	}

	commandRunner := c.CommandRunner
	if commandRunner == nil {
		commandRunner = exec.CommandContext
	}

	// --include-partial-messages enables the per-token stream_event envelopes
	// (message_start, content_block_delta, etc.) that PhaseFirstToken and
	// PhaseGenerating are dispatched from. Without it, Claude only emits the
	// coarse `assistant` message + final `result` event, and the progress UI
	// stays silent between "Generating checkpoint summary..." and "✓ done".
	cmd := commandRunner(ctx, "claude",
		"--print",
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--model", model,
		"--setting-sources", "")

	cmd.Dir = os.TempDir()
	cmd.Env = agent.StripGitEnv(os.Environ())
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("claude stream stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("claude stream start: %w", err)
	}

	final, malformed, parseErr := streamClaudeResponse(stdout, makeProgressDispatcher(progress))

	// Drain any unread stdout so the subprocess can exit cleanly even if the
	// scanner aborted early (e.g. bufio.ErrTooLong on an oversized line).
	// Without this, a blocked pipe would deadlock cmd.Wait().
	if _, drainErr := io.Copy(io.Discard, stdout); drainErr != nil {
		logging.Debug(ctx, "draining claude stream stdout", slog.String("error", drainErr.Error()))
	}
	waitErr := cmd.Wait()

	if malformed > 0 {
		logging.Warn(ctx, "skipped malformed claude stream lines", slog.Int("count", malformed))
	}

	// Specific envelope error outranks a generic ctx-cancel message.
	if final != nil && final.IsError {
		return "", envelopeErrorMessage(final)
	}

	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", context.DeadlineExceeded
		}
		return "", context.Canceled
	}

	if final != nil {
		if final.Result == nil {
			return "", errors.New("claude returned empty result")
		}
		if progress != nil {
			progress(agent.GenerationProgress{
				Phase:        agent.PhaseDone,
				OutputTokens: outputTokensFromUsage(final.Usage),
				DurationMs:   final.DurationMs,
			})
		}
		return *final.Result, nil
	}

	// No envelope: check if the CLI rejected streaming flags (older version).
	if waitErr != nil {
		stderrStr := stderr.String()
		if looksLikeUnrecognizedFlag(stderrStr) {
			logging.Warn(ctx, "claude CLI rejected stream-json flags; falling back to non-streaming (no progress output)",
				slog.String("stderr", strings.TrimSpace(stderrStr)))
			return c.GenerateText(ctx, prompt, model)
		}
		if stderrStr != "" {
			return "", fmt.Errorf("claude stream failed: %s: %w", strings.TrimSpace(stderrStr), waitErr)
		}
		return "", fmt.Errorf("claude stream failed: %w", waitErr)
	}

	if parseErr != nil {
		return "", fmt.Errorf("claude stream parse: %w", parseErr)
	}
	return "", errors.New("claude exited without producing a result")
}

// envelopeErrorMessage formats an is_error result envelope as a user-facing
// error. Preserves the CLI's result text and HTTP status when present.
func envelopeErrorMessage(final *streamEvent) error {
	msg := "claude CLI reported error"
	if final.Result != nil && *final.Result != "" {
		msg = fmt.Sprintf("%s: %s", msg, *final.Result)
	}
	if final.APIErrorStatus != nil {
		msg = fmt.Sprintf("%s (HTTP %d)", msg, *final.APIErrorStatus)
	}
	return errors.New(msg)
}

// makeProgressDispatcher returns a per-event handler that translates raw
// stream events into agent.GenerationProgress callbacks. PhaseDone is
// emitted by GenerateTextStreaming after cmd.Wait, because it needs data
// from the parsed final envelope.
func makeProgressDispatcher(progress agent.ProgressFn) func(streamEvent) {
	if progress == nil {
		return func(streamEvent) {}
	}
	var outputTokensEstimate int
	return func(ev streamEvent) {
		switch {
		case ev.Type == "system" && ev.Subtype == "status" && ev.Status == "requesting":
			progress(agent.GenerationProgress{Phase: agent.PhaseConnecting})
		case ev.Type == streamEventTypeStreamEvent && ev.Event.Type == "message_start":
			p := agent.GenerationProgress{Phase: agent.PhaseFirstToken, TTFTms: ev.TTFTms}
			if ev.Event.Message != nil && ev.Event.Message.Usage != nil {
				p.InputTokens = ev.Event.Message.Usage.InputTokens
				p.CachedInputTokens = ev.Event.Message.Usage.CacheReadInputTokens
			}
			progress(p)
		case ev.Type == streamEventTypeStreamEvent && ev.Event.Type == "content_block_delta" && ev.Event.Delta != nil:
			text := ev.Event.Delta.Text
			if text == "" {
				text = ev.Event.Delta.Thinking
			}
			outputTokensEstimate += len(text) / 4 // rough estimate: ~4 chars/token
			progress(agent.GenerationProgress{Phase: agent.PhaseGenerating, OutputTokens: outputTokensEstimate})
		}
	}
}

func outputTokensFromUsage(u *messageUsage) int {
	if u == nil {
		return 0
	}
	return u.OutputTokens
}

// looksLikeUnrecognizedFlag returns true if stderr indicates the CLI
// rejected one of the streaming-specific flags (older Claude CLI). Requires
// both a rejection phrase AND a streaming flag name to avoid false-positives
// on unrelated errors that happen to contain "unknown option".
func looksLikeUnrecognizedFlag(stderr string) bool {
	lower := strings.ToLower(stderr)
	hasRejectPhrase := strings.Contains(lower, "unrecognized option") ||
		strings.Contains(lower, "unknown flag") ||
		strings.Contains(lower, "unknown option") ||
		strings.Contains(lower, "invalid option")
	if !hasRejectPhrase {
		return false
	}
	return strings.Contains(lower, "stream-json") ||
		strings.Contains(lower, "verbose") ||
		strings.Contains(lower, "include-partial")
}
