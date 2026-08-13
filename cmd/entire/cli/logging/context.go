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
	loggerKey
)

// WithLogger attaches an initialized logger to the context. Init calls this
// on the context it returns; entry points (cobra PreRun/RunE) pass that
// context down so downstream code receives the logger by injection instead
// of probing package state for readiness.
//
// The logger is a snapshot bound to the log file Init opened: it stays valid
// until the next Init or Close, after which its writes land on a closed file
// and are silently discarded. Entry points initialize once per process, so
// don't stash this logger anywhere that outlives the command.
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
