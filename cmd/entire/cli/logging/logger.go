// Package logging provides structured logging for the Entire CLI using slog.
//
// A Logger owns one log file. Construct it once at the entry point, put it in
// the context, and close it there — nothing in this package is process-global,
// so two loggers can coexist and a command cannot be affected by what another
// one configured.
//
// Usage:
//
//	// At the cobra entry point: build the logger and inject it, so downstream
//	// code receives it from the context.
//	l, err := logging.New(logging.Config{Dir: dir, Level: level})
//	if err != nil {
//	    // handle error
//	}
//	ctx = logging.WithLogger(ctx, l)
//
//	// At the exit point, from the context rather than package state.
//	defer func() {
//	    if l := logging.LoggerFromContext(ctx); l != nil {
//	        _ = l.Close()
//	    }
//	}()
//
//	// Add context values
//	ctx = logging.WithSessionID(ctx, sessionID)
//	ctx = logging.WithComponent(ctx, "hooks")
//	ctx = logging.WithAgent(ctx, agentName)
//
//	// Log with context - session/component/agent extracted automatically
//	logging.Info(ctx, "hook invoked",
//	    slog.String("hook", hookName),
//	    slog.String("branch", branch),
//	)
package logging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LogLevelEnvVar is the environment variable that controls log level.
const LogLevelEnvVar = "ENTIRE_LOG_LEVEL"

// LogsDir is the directory where log files are stored (relative to repo root).
const LogsDir = ".entire/logs"

// logFileName is the single file every Logger appends to inside Config.Dir.
const logFileName = "entire.log"

// writeBufferSize batches writes so a hook doing many small log calls pays one
// syscall per 8KB rather than one per line.
const writeBufferSize = 8192

// Config describes a Logger to build.
type Config struct {
	// Dir is the directory to create the log file in, created if absent.
	// Required: an empty Dir is an error rather than a silent default, because
	// the caller is the only thing that knows whether writing here is allowed
	// (never outside a repository, never in a repo that has not enabled Entire).
	Dir string

	// Level is the minimum level to emit. The zero value is slog.LevelInfo.
	// Resolve it from the environment and settings with ParseLevel.
	Level slog.Level
}

// Logger owns a log file, the buffered writer in front of it, and the slog
// handler that writes through them.
//
// Safe for concurrent use: slog handlers may be called from several goroutines
// and bufio.Writer is not goroutine-safe, so every write and Close is
// serialized. Writes after Close are dropped rather than failing — losing a log
// line must never surface as an error in the caller.
type Logger struct {
	slog *slog.Logger

	// mu serializes writes to buf and guards the buf/file handles against a
	// concurrent Close. Exclusive rather than an RWMutex: concurrent writers are
	// exactly the problem, so admitting them together would defeat it.
	mu   sync.Mutex
	buf  *bufio.Writer
	file *os.File
}

// New opens cfg.Dir/entire.log for appending and returns a Logger writing JSON
// to it.
//
// Errors are real errors: the caller decides whether a missing log file is
// fatal (it generally is not) and what to do instead. Nothing falls back to
// stderr here, because a logger that writes to the terminal would splash
// operational lines over the user's output once injected.
func New(cfg Config) (*Logger, error) {
	if cfg.Dir == "" {
		return nil, errors.New("logging: Config.Dir is required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	path := filepath.Join(cfg.Dir, logFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // fixed filename under a caller-vetted directory
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	l := &Logger{
		buf:  bufio.NewWriterSize(f, writeBufferSize),
		file: f,
	}
	l.slog = slog.New(slog.NewJSONHandler(logWriter{l}, &slog.HandlerOptions{Level: cfg.Level}))
	return l, nil
}

// Slog returns the underlying *slog.Logger, for packages that take one by
// injection (redact) and should not depend on this wrapper type.
func (l *Logger) Slog() *slog.Logger {
	if l == nil {
		return nil
	}
	return l.slog
}

// Close flushes the buffer and closes the log file. Idempotent, and safe to
// call concurrently with logging: later writes are dropped.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var flushErr error
	if l.buf != nil {
		if err := l.buf.Flush(); err != nil {
			flushErr = fmt.Errorf("flush log buffer: %w", err)
		}
		l.buf = nil
	}
	if l.file != nil {
		if err := l.file.Close(); err != nil && flushErr == nil {
			flushErr = fmt.Errorf("close log file: %w", err)
		}
		l.file = nil
	}
	return flushErr
}

// logWriter adapts a Logger to io.Writer for its slog handler without putting
// Write on Logger's own public surface.
type logWriter struct{ l *Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.l.mu.Lock()
	defer w.l.mu.Unlock()

	if w.l.buf == nil {
		return len(p), nil
	}
	n, err := w.l.buf.Write(p)
	if err != nil {
		return n, fmt.Errorf("write to log buffer: %w", err)
	}
	return n, nil
}

// ParseLevel maps a log level name to a slog.Level. ok is false for a non-empty
// name that is not recognized, so the caller can warn about a typo rather than
// silently logging at the default level. An empty name is the default, INFO,
// and reports ok.
func ParseLevel(s string) (level slog.Level, ok bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "":
		return slog.LevelInfo, true
	case "DEBUG":
		return slog.LevelDebug, true
	case "INFO":
		return slog.LevelInfo, true
	case "WARN", "WARNING":
		return slog.LevelWarn, true
	case "ERROR":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

// Debug logs at DEBUG level with context values automatically extracted.
func Debug(ctx context.Context, msg string, attrs ...any) {
	log(ctx, slog.LevelDebug, msg, attrs...)
}

// Info logs at INFO level with context values automatically extracted.
func Info(ctx context.Context, msg string, attrs ...any) {
	log(ctx, slog.LevelInfo, msg, attrs...)
}

// Warn logs at WARN level with context values automatically extracted.
func Warn(ctx context.Context, msg string, attrs ...any) {
	log(ctx, slog.LevelWarn, msg, attrs...)
}

// Error logs at ERROR level with context values automatically extracted.
func Error(ctx context.Context, msg string, attrs ...any) {
	log(ctx, slog.LevelError, msg, attrs...)
}

// log is the internal logging function that extracts context values and logs.
//
// The logger comes from the context, which is the same logger downstream
// packages receive by injection — one resolution path, so a caller cannot log
// somewhere other than where redact does. slog.Default() is the fallback for a
// context built before the entry point ran or derived from context.Background();
// that one is the runtime's own global, not state this package owns.
//
// Context attributes lose to caller-supplied ones: slog does not dedupe attrs,
// so emitting session_id from the context on top of a call site that passes its
// own would put the key in the line twice. Over a hundred call sites pass one
// by hand, so the collision is the common case, not the corner.
func log(ctx context.Context, level slog.Level, msg string, attrs ...any) {
	l := LoggerFromContext(ctx).Slog()
	if l == nil {
		l = slog.Default()
	}

	contextAttrs := attrsFromContext(ctx)
	dropSessionID := hasSessionIDAttr(attrs)

	allAttrs := make([]any, 0, len(contextAttrs)+len(attrs))
	for _, a := range contextAttrs {
		if dropSessionID && a.Key == sessionIDAttrKey {
			continue
		}
		allAttrs = append(allAttrs, a)
	}
	allAttrs = append(allAttrs, attrs...)

	// Pass nil context to slog as we've already extracted context values as attributes.
	// slog handlers are expected to handle nil context gracefully.
	l.Log(nil, level, msg, allAttrs...) //nolint:staticcheck // nil context is intentional - we extract values as attributes
}

// hasSessionIDAttr reports whether attrs already carry a session_id.
//
// attrs follows slog's calling convention: slog.Attr values and loose
// key/value pairs may be mixed, where a string element is a key and the
// element after it its value. A session_id nested inside a slog.Group is a
// different JSON path and so does not collide.
func hasSessionIDAttr(attrs []any) bool {
	for i := 0; i < len(attrs); i++ {
		switch a := attrs[i].(type) {
		case slog.Attr:
			if a.Key == sessionIDAttrKey {
				return true
			}
		case string:
			if a == sessionIDAttrKey {
				return true
			}
			i++ // the next element is this key's value, not a key
		}
	}
	return false
}

// attrsFromContext extracts logging attributes from a context.
func attrsFromContext(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}

	var attrs []slog.Attr

	// Session first, matching the order log lines have always had.
	if s := sessionIDFromContext(ctx); s != "" {
		attrs = append(attrs, slog.String(sessionIDAttrKey, s))
	}
	if v := ctx.Value(componentKey); v != nil {
		if s, ok := v.(string); ok && s != "" {
			attrs = append(attrs, slog.String("component", s))
		}
	}
	if v := ctx.Value(agentKey); v != nil {
		if s, ok := v.(string); ok && s != "" {
			attrs = append(attrs, slog.String("agent", s))
		}
	}

	return attrs
}
