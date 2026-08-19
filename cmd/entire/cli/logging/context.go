package logging

import (
	"context"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// Context keys for logging values.
// Using private types to avoid key collisions.
type contextKey int

const (
	componentKey contextKey = iota
	agentKey
	sessionKey
	loggerKey
)

// sessionIDAttrKey is the log attribute the session context value emits under.
// Named because log() also has to recognize it among caller-supplied attrs
// (see log).
const sessionIDAttrKey = "session_id"

// WithLogger attaches a Logger to the context. Entry points (cobra
// PreRun/RunE) build one with New and pass the resulting context down so
// downstream code receives the logger by injection instead of probing package
// state for readiness.
//
// The exit point closes it by reading it back out of the context — the logger
// has no other owner — so don't stash it anywhere that outlives the command.
func WithLogger(ctx context.Context, l *Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// LoggerFromContext returns the Logger attached by WithLogger, or nil when the
// entry point never set up file-backed logging. A nil result is the signal to
// skip log lines that would otherwise fall through to the stderr default logger
// and surface as terminal noise. Its methods are nil-safe, so a nil result can
// be closed or asked for its Slog without a guard.
func LoggerFromContext(ctx context.Context) *Logger {
	if ctx == nil {
		return nil
	}
	if l, ok := ctx.Value(loggerKey).(*Logger); ok {
		return l
	}
	return nil
}

// WithSessionID adds a session ID to the context, so every line logged under
// it is filterable by the agent session that produced it.
//
// Use this when the session ID becomes known — the hook path resolves it after
// the entry point has already initialized logging — rather than calling Init
// again. The log file path is fixed, so a re-Init would close and reopen the
// same file and rebuild its 8KB buffer purely to add this one attribute.
//
// Like WithComponent and WithAgent, this is a plain context value, so calling
// it again on a derived context shadows the outer session for that scope only.
// It deliberately does not validate: logging builds no paths from the session
// ID, it is an slog attribute. Guarding traversal belongs where the ID is
// resolved from the filesystem, not here.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionKey, sessionID)
}

// SessionLoggerFromContext returns the context's logger stamped with the
// context's session ID, for packages that hold an injected *slog.Logger and
// call it without a context — redact's redaction diagnostics, which are
// exactly the lines a user grepping for their session needs to find. Those
// calls never reach log(), so they cannot pick the attribute up from the
// context themselves. Nil when there is no injected logger, so callers keep
// treating that as "logging is not file-backed".
//
// Only the session is stamped. component is deliberately left off: redact tags
// its own lines with component=redaction, and slog does not dedupe attrs, so
// re-adding it from the context would emit the key twice.
func SessionLoggerFromContext(ctx context.Context) *slog.Logger {
	l := LoggerFromContext(ctx).Slog()
	if l == nil {
		return nil
	}
	if sessionID := sessionIDFromContext(ctx); sessionID != "" {
		return l.With(slog.String(sessionIDAttrKey, sessionID))
	}
	return l
}

// sessionIDFromContext returns the session ID attached by WithSessionID, or "".
func sessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if s, ok := ctx.Value(sessionKey).(string); ok {
		return s
	}
	return ""
}

// WithComponent adds a component name to the context.
// Component names help identify the subsystem generating logs (e.g., "hooks", "strategy", "session").
func WithComponent(ctx context.Context, component string) context.Context {
	return context.WithValue(ctx, componentKey, component)
}

// WithAgent adds an agent name to the context.
// Agent names identify the AI agent generating activity (e.g., "claude-code", "cursor", "aider").
func WithAgent(ctx context.Context, agentName types.AgentName) context.Context {
	return context.WithValue(ctx, agentKey, string(agentName))
}
