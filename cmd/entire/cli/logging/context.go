package logging

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/validation"
)

// Context keys for logging values.
// Using private types to avoid key collisions.
type contextKey int

const (
	componentKey contextKey = iota
	agentKey
	loggerKey
)

// WithLogger attaches an initialized logger to the context. Init calls this
// on the context it returns; entry points (cobra PreRun/RunE) pass that
// context down so downstream code receives the logger by injection instead
// of probing package state for readiness.
//
// The logger writes through an indirection onto whichever log file Init most
// recently opened (see liveWriter), so it stays valid across a re-Init and is
// safe to hold and to write to concurrently. After Close there is no file and
// its writes are dropped, so don't stash it anywhere that outlives the command.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// LoggerFromContext returns the logger attached by WithLogger, or nil when
// the entry point never initialized file-backed logging (including Init's
// stderr fallback). A nil result is the signal to skip log lines that would
// otherwise fall through to a stderr logger and surface as terminal noise.
func LoggerFromContext(ctx context.Context) *slog.Logger {
	if ctx == nil {
		return nil
	}
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return nil
}

// WithSessionID returns ctx carrying a logger stamped with sessionID, so both
// the package-level helpers and direct holders of the injected logger (the
// redact package) emit lines filterable by session.
//
// Use this when the session ID becomes known — the hook path resolves it after
// the entry point has already initialized logging — rather than calling Init
// again. The log file path is fixed, so a re-Init would close and reopen the
// same file and rebuild its 8KB buffer purely to add this one attribute.
//
// Unlike WithComponent and WithAgent, this stamps the logger rather than
// storing a context value: redact holds the logger directly and would
// otherwise lose session_id from exactly the diagnostics where it matters
// most. Package state is updated too, so code that logs through a context with
// no injected logger is still stamped.
//
// Returns ctx unchanged when logging is not file-backed — there is no logger to
// stamp, and injecting the stderr one would break the invariant that a context
// logger means "writes go to .entire/logs/".
func WithSessionID(ctx context.Context, sessionID string) (context.Context, error) {
	if sessionID == "" {
		return ctx, nil
	}
	if err := validation.ValidateSessionID(sessionID); err != nil {
		return ctx, fmt.Errorf("invalid session ID for logging: %w", err)
	}

	mu.Lock()
	currentSessionID = sessionID
	mu.Unlock()

	l := LoggerFromContext(ctx)
	if l == nil {
		return ctx, nil
	}
	return WithLogger(ctx, l.With(slog.String("session_id", sessionID))), nil
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
