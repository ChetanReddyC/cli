// Package logging provides structured logging for the Entire CLI using slog.
//
// Usage:
//
//	// Initialize logging (typically at the cobra entry point) and inject the
//	// logger it returns, so downstream code receives it from the context.
//	l, err := logging.Init(ctx)
//	if err != nil {
//	    // handle error
//	}
//	if l != nil {
//	    ctx = logging.WithLogger(ctx, l)
//	}
//	defer logging.Close()
//
//	// Later, once the session ID is known (no need to re-Init)
//	ctx, err = logging.WithSessionID(ctx, sessionID)
//
//	// Add context values
//	ctx = logging.WithComponent(ctx, "hooks")
//	ctx = logging.WithAgent(ctx, agentName)
//
//	// Log with context - component/agent extracted automatically
//	logging.Info(ctx, "hook invoked",
//	    slog.String("hook", hookName),
//	    slog.String("branch", branch),
//	)
package logging

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// LogLevelEnvVar is the environment variable that controls log level.
const LogLevelEnvVar = "ENTIRE_LOG_LEVEL"

// LogsDir is the directory where log files are stored (relative to repo root).
const LogsDir = ".entire/logs"

var (
	// logger is the package-level logger instance
	logger *slog.Logger

	// logFile holds the current log file handle for cleanup
	logFile *os.File

	// logBufWriter wraps logFile with buffered I/O for performance
	logBufWriter *bufio.Writer

	// currentSessionID stores the session ID from WithSessionID to include in
	// log lines whose context carries no injected logger
	currentSessionID string

	// mu protects logger, logFile, logBufWriter, and currentSessionID, and
	// serializes writes to logBufWriter (see liveWriter)
	mu sync.RWMutex

	// logLevelGetter is an optional callback to get log level from settings.
	// Set by SetLogLevelGetter before Init is called.
	logLevelGetter func() string
)

// liveWriter routes each write to whichever buffered log file Init most
// recently opened, resolved per write under the read lock. Loggers Init hands
// out — the package-level one and the copy it puts in the context — are bound
// to this indirection rather than straight to a *bufio.Writer, which buys two
// things:
//
//   - They survive a second Init. Init flushes and drops the previous
//     bufio.Writer, so a logger holding that object directly would keep writing
//     into an orphaned buffer nothing ever flushes, silently losing every line.
//     Nothing re-initializes in a live process today — attaching a session ID
//     goes through WithSessionID precisely so it need not — but the indirection
//     keeps that from being a landmine for whoever calls Init twice next.
//   - Writes are serialized. bufio.Writer is not goroutine-safe, so two
//     goroutines logging at once — or one logging while Close flushes — corrupt
//     the buffer. The lock is exclusive, not a read lock, precisely because
//     concurrent writers are the problem; a read lock would admit them all at
//     once. This also covers holders of the logger that never go through log(),
//     which is how the redact package uses the injected one.
//
// After Close there is no writer, and writes are dropped rather than erroring:
// losing a log line must never surface as a failure in the caller.
//
// Init and Close hold the same lock, so nothing they call may log through here.
// Today neither does — Init's invalid-level warning goes straight to os.Stderr.
type liveWriter struct{}

func (liveWriter) Write(p []byte) (int, error) {
	mu.Lock()
	defer mu.Unlock()

	if logBufWriter == nil {
		return len(p), nil
	}
	n, err := logBufWriter.Write(p)
	if err != nil {
		return n, fmt.Errorf("write to log buffer: %w", err)
	}
	return n, nil
}

// SetLogLevelGetter sets a callback function to get the log level from settings.
// This allows the logging package to read settings without a circular dependency.
// The callback is only used if ENTIRE_LOG_LEVEL env var is not set.
func SetLogLevelGetter(getter func() string) {
	mu.Lock()
	defer mu.Unlock()
	logLevelGetter = getter
}

// Init opens the log sink, writing JSON logs to .entire/logs/entire.log.
//
// If the log file cannot be created, falls back to stderr.
// Log level is controlled by ENTIRE_LOG_LEVEL environment variable.
//
// Returns the initialized logger for the caller to put in its context with
// WithLogger — Init sets up the sink, the caller decides where the handle
// lives. Packages that take an injected *slog.Logger, like redact, then
// receive it from that context.
//
// Init takes no session ID: the log path is fixed, so there is nothing about a
// session for it to act on. Attach one with WithSessionID when it becomes
// known, which on the hook path is after this has already run.
//
// A nil logger with a nil error means logging is not file-backed: Init fell
// back to stderr. Callers must not inject that, because a logger in the
// context asserts "writes go to .entire/logs/", never to the terminal.
func Init(ctx context.Context) (*slog.Logger, error) {
	mu.Lock()
	defer mu.Unlock()

	// Close any existing log file (flush buffer first)
	if logBufWriter != nil {
		_ = logBufWriter.Flush()
		logBufWriter = nil
	}
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}

	// Get log level from environment first, then settings
	levelStr := os.Getenv(LogLevelEnvVar)
	if levelStr == "" && logLevelGetter != nil {
		levelStr = logLevelGetter()
	}
	level := parseLogLevel(levelStr)

	// Warn if invalid level was provided
	if levelStr != "" && !isValidLogLevel(levelStr) {
		fmt.Fprintf(os.Stderr, "[entire] Warning: invalid log level %q, defaulting to INFO\n", levelStr)
	}

	// Determine log file path
	repoRoot, err := paths.WorktreeRoot(ctx)
	if err != nil {
		// Fall back to current directory
		repoRoot = "."
	}

	logsPath := filepath.Join(repoRoot, LogsDir)
	if err := os.MkdirAll(logsPath, 0o750); err != nil {
		// Fall back to stderr for the package-level helpers, but hand the
		// caller nothing to inject.
		logger = createLogger(os.Stderr, level)
		//nolint:nilnil // Documented signal, not an oversight: no logger and no
		// error means "initialized, but not file-backed". It cannot be an error
		// — callers must still proceed (the hook path configures redaction here
		// regardless) — and it must not be the stderr logger, which callers
		// would then inject and splash INFO lines onto the terminal.
		return nil, nil
	}

	logFilePath := filepath.Join(logsPath, "entire.log")
	f, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // fixed filename, not user-controlled
	if err != nil {
		// Fall back to stderr (see the fallback above for why nil, nil).
		logger = createLogger(os.Stderr, level)
		//nolint:nilnil // see above
		return nil, nil
	}

	logFile = f
	logBufWriter = bufio.NewWriterSize(f, 8192) // 8KB buffer for batched writes
	logger = createLogger(liveWriter{}, level)
	// A fresh sink carries no session yet; WithSessionID sets this.
	currentSessionID = ""

	return logger, nil
}

// Close closes the log file if one is open.
// Flushes any buffered data before closing.
// Safe to call multiple times.
func Close() {
	mu.Lock()
	defer mu.Unlock()

	if logBufWriter != nil {
		_ = logBufWriter.Flush()
		logBufWriter = nil
	}
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	currentSessionID = ""
}

// resetLogger resets the logger to nil (for testing).
func resetLogger() {
	mu.Lock()
	defer mu.Unlock()
	logger = nil
	currentSessionID = ""
	if logBufWriter != nil {
		_ = logBufWriter.Flush()
		logBufWriter = nil
	}
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
}

// createLogger creates a JSON logger writing to the given writer at the specified level.
func createLogger(w io.Writer, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: level,
	}
	handler := slog.NewJSONHandler(w, opts)
	return slog.New(handler)
}

// parseLogLevel parses a log level string to slog.Level.
// Returns slog.LevelInfo for empty or invalid values.
func parseLogLevel(s string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG":
		return slog.LevelDebug
	case "INFO":
		return slog.LevelInfo
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// isValidLogLevel checks if the given string is a valid log level.
func isValidLogLevel(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DEBUG", "INFO", "WARN", "WARNING", "ERROR", "":
		return true
	default:
		return false
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
// The logger comes from the context when the entry point put one there
// (logging.Init → WithLogger), which is the same logger downstream packages
// receive by injection — one resolution path, so a caller cannot log somewhere
// other than where redact does. The package-level logger is the fallback for
// contexts built before Init or derived from context.Background(), and
// slog.Default() the last resort when nothing initialized logging at all.
//
// No lock is held across l.Log: writes reach the log file through liveWriter,
// which takes the read lock per write, so Init/Close cannot flush the buffer
// mid-write. Only the globals read below need synchronizing.
func log(ctx context.Context, level slog.Level, msg string, attrs ...any) {
	mu.RLock()
	l := logger
	globalSessionID := currentSessionID
	mu.RUnlock()

	// Init already stamped session_id onto the logger it put in the context, so
	// taking that path must not add it again or every line carries it twice.
	stampSessionID := true
	if ctxLogger := LoggerFromContext(ctx); ctxLogger != nil {
		l = ctxLogger
		stampSessionID = false
	}
	if l == nil {
		l = slog.Default()
	}

	// Build attributes slice with session ID first (if set)
	var allAttrs []any

	// Add session ID from Init() if set (always first for consistency)
	if stampSessionID && globalSessionID != "" {
		allAttrs = append(allAttrs, slog.String("session_id", globalSessionID))
	}

	// Extract context values
	contextAttrs := attrsFromContext(ctx)
	for _, a := range contextAttrs {
		allAttrs = append(allAttrs, a)
	}

	// Add caller-provided attributes
	allAttrs = append(allAttrs, attrs...)

	// Pass nil context to slog as we've already extracted context values as attributes.
	// slog handlers are expected to handle nil context gracefully.
	l.Log(nil, level, msg, allAttrs...) //nolint:staticcheck // nil context is intentional - we extract values as attributes
}

// attrsFromContext extracts logging attributes from a context.
func attrsFromContext(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}

	var attrs []slog.Attr

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
