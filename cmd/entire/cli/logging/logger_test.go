package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Test constants to avoid goconst warnings
const (
	testSessionID = "2025-01-15-test-session"
	testComponent = "hooks"
	testAgent     = "claude-code"
)

// testLogFilePath returns the expected log file path for a test temp directory.
func testLogFilePath(tmpDir string) string {
	return filepath.Join(tmpDir, ".entire", "logs", "entire.log")
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		want     slog.Level
	}{
		{"empty defaults to INFO", "", slog.LevelInfo},
		{"DEBUG lowercase", "debug", slog.LevelDebug},
		{"DEBUG uppercase", "DEBUG", slog.LevelDebug},
		{"INFO lowercase", "info", slog.LevelInfo},
		{"INFO uppercase", "INFO", slog.LevelInfo},
		{"WARN lowercase", "warn", slog.LevelWarn},
		{"WARN uppercase", "WARN", slog.LevelWarn},
		{"ERROR lowercase", "error", slog.LevelError},
		{"ERROR uppercase", "ERROR", slog.LevelError},
		{"invalid defaults to INFO", "invalid", slog.LevelInfo},
		{"warning alias", "warning", slog.LevelWarn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLogLevel(tt.envValue)
			if got != tt.want {
				t.Errorf("parseLogLevel(%q) = %v, want %v", tt.envValue, got, tt.want)
			}
		})
	}
}

func TestInit_CreatesLogDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Initialize git repo so WorktreeRoot works
	initGitRepo(t, tmpDir)

	_, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer Close()

	logsDir := filepath.Join(tmpDir, ".entire", "logs")
	if _, err := os.Stat(logsDir); os.IsNotExist(err) {
		t.Errorf("Init() did not create .entire/logs/ directory")
	}
}

func TestInit_CreatesLogFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	_, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer Close()

	if _, err := os.Stat(testLogFilePath(tmpDir)); os.IsNotExist(err) {
		t.Errorf("Init() did not create log file at %s", testLogFilePath(tmpDir))
	}
}

// TestInit_ReturnsLoggerForInjection pins the injection contract downstream
// consumers rely on (strategy hands the context's logger to the redact
// package, and gates the redaction summary on it being non-nil): the logger
// Init returns must write to .entire/logs/entire.log and must survive the round
// trip through WithLogger/SessionLoggerFromContext, which stamps the context's
// session onto it for callers that log without a context.
func TestInit_ReturnsLoggerForInjection(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	initialized, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if initialized == nil {
		t.Fatal("Init() returned no logger for a writable log directory")
	}

	ctx := WithSessionID(WithLogger(context.Background(), initialized), testSessionID)
	l := SessionLoggerFromContext(ctx)
	if l == nil {
		t.Fatal("SessionLoggerFromContext() = nil for a context holding the initialized logger")
	}
	l.Warn("injected logger writes to the log file")

	// Close to flush
	Close()

	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}
	if !strings.Contains(string(content), "injected logger writes to the log file") {
		t.Errorf("log line written via the context logger missing from log file: %s", content)
	}
	if !strings.Contains(string(content), `"session_id":"`+testSessionID+`"`) {
		t.Errorf("context logger line missing stamped session_id: %s", content)
	}

	if LoggerFromContext(context.Background()) != nil {
		t.Error("LoggerFromContext() should be nil for a context without Init")
	}
	if SessionLoggerFromContext(context.Background()) != nil {
		t.Error("SessionLoggerFromContext() should be nil for a context without Init")
	}
}

// TestInit_StderrFallbackReturnsNoLogger pins the summary-gate contract for the
// fallback path: when Init cannot open .entire/logs/ and falls back to stderr,
// it must return a nil logger so callers have nothing to inject. Injecting one
// would fire the redaction summary gate (logger != nil) and splash a JSON INFO
// line onto the user's terminal on every commit.
func TestInit_StderrFallbackReturnsNoLogger(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	// A regular file at .entire makes MkdirAll(".entire/logs") fail,
	// forcing the stderr fallback.
	if err := os.WriteFile(filepath.Join(tmpDir, ".entire"), []byte("not a dir"), 0o600); err != nil {
		t.Fatalf("failed to block .entire dir: %v", err)
	}

	l, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v (fallback must not be an error)", err)
	}
	defer Close()

	if l != nil {
		t.Error("Init() must return a nil logger on the stderr fallback path")
	}
}

func TestInit_WritesJSONLogs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	_, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Log something
	Info(context.Background(), "test message", slog.String("key", "value"))

	// Close to flush
	Close()

	// Read log file
	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal(content, &logEntry); err != nil {
		t.Errorf("Log output is not valid JSON: %v\nContent: %s", err, content)
	}

	// Verify expected fields
	if msg, ok := logEntry["msg"].(string); !ok || msg != "test message" {
		t.Errorf("Expected msg='test message', got %v", logEntry["msg"])
	}
	if key, ok := logEntry["key"].(string); !ok || key != "value" {
		t.Errorf("Expected key='value', got %v", logEntry["key"])
	}
	if _, ok := logEntry["time"]; !ok {
		t.Error("Expected 'time' field in log entry")
	}
	if _, ok := logEntry["level"]; !ok {
		t.Error("Expected 'level' field in log entry")
	}
}

func TestInit_RespectsLogLevel(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	// Set log level to WARN
	t.Setenv(LogLevelEnvVar, "WARN")

	_, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	ctx := context.Background()

	// These should NOT be logged
	Debug(ctx, "debug message")
	Info(ctx, "info message")

	// This SHOULD be logged
	Warn(ctx, "warn message")

	Close()

	// Read log file
	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	contentStr := string(content)
	if strings.Contains(contentStr, "debug message") {
		t.Error("DEBUG message should not be logged when level is WARN")
	}
	if strings.Contains(contentStr, "info message") {
		t.Error("INFO message should not be logged when level is WARN")
	}
	if !strings.Contains(contentStr, "warn message") {
		t.Error("WARN message should be logged when level is WARN")
	}
}

func TestInit_InvalidLogLevelWarns(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	// Capture stderr
	var buf bytes.Buffer
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Failed to create pipe: %v", err)
	}
	os.Stderr = w

	t.Setenv(LogLevelEnvVar, "INVALID_LEVEL")

	_, err = Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	w.Close()
	os.Stderr = oldStderr

	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("Failed to read from pipe: %v", err)
	}
	stderrOutput := buf.String()

	if !strings.Contains(stderrOutput, "invalid log level") {
		t.Errorf("Expected warning about invalid log level on stderr, got: %s", stderrOutput)
	}

	Close()
}

func TestInit_FallsBackToStderrOnError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	// Make logs directory unwritable (simulate permission error)
	logsDir := filepath.Join(tmpDir, ".entire", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("Failed to create logs dir: %v", err)
	}

	// Create a directory with the same name as the log file to cause an error
	if err := os.MkdirAll(testLogFilePath(tmpDir), 0o755); err != nil {
		t.Fatalf("Failed to create blocking dir: %v", err)
	}

	// Init should not return error, but fall back to stderr
	_, err := Init(context.Background())
	if err != nil {
		t.Errorf("Init() should not error, but got: %v", err)
	}

	// Verify logger still works (writing to stderr)
	Info(context.Background(), "fallback test")

	Close()
}

func TestClose_SafeToCallMultipleTimes(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	_, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Should not panic
	Close()
	Close()
	Close()
}

func TestLogging_BeforeInit(_ *testing.T) {
	// Reset any global state
	resetLogger()

	// These should not panic, should use default stderr logger
	ctx := context.Background()
	Debug(ctx, "debug before init")
	Info(ctx, "info before init")
	Warn(ctx, "warn before init")
	Error(ctx, "error before init")
}

// Helper to initialize a git repo for tests
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
	cmd := "git init && git config user.email 'test@test.com' && git config user.name 'Test'"
	output, err := execCommand(t, "sh", "-c", cmd)
	if err != nil {
		t.Fatalf("Failed to init git repo: %v\nOutput: %s", err, output)
	}
}

func execCommand(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestLogging_IncludesContextValues(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	sessionID := "2025-01-15-context-test"
	_, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// Create context with values.
	ctx := WithSessionID(context.Background(), sessionID)
	ctx = WithComponent(ctx, testComponent)
	ctx = WithAgent(ctx, testAgent)

	// Log with context
	Info(ctx, "context test message")

	Close()

	// Read log file
	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal(content, &logEntry); err != nil {
		t.Fatalf("Log output is not valid JSON: %v\nContent: %s", err, content)
	}

	// session_id comes from WithSessionID
	if logEntry["session_id"] != sessionID {
		t.Errorf("Expected session_id='%s' (from WithSessionID), got %v", sessionID, logEntry["session_id"])
	}
	if logEntry["component"] != testComponent {
		t.Errorf("Expected component='%s', got %v", testComponent, logEntry["component"])
	}
	if logEntry["agent"] != testAgent {
		t.Errorf("Expected agent='%s', got %v", testAgent, logEntry["agent"])
	}
}

func TestLogging_AdditionalAttrs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	initGitRepo(t, tmpDir)

	sessionID := "2025-01-15-attrs-test"
	_, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	ctx := WithSessionID(context.Background(), sessionID)

	// Log with additional attrs
	Info(ctx, "attrs test",
		slog.String("hook", "pre-push"),
		slog.Int("duration_ms", 150),
		slog.Bool("success", true),
	)

	Close()

	// Read log file
	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}

	// Parse as JSON
	var logEntry map[string]interface{}
	if err := json.Unmarshal(content, &logEntry); err != nil {
		t.Fatalf("Log output is not valid JSON: %v\nContent: %s", err, content)
	}

	// session_id comes from WithSessionID, additional attrs work alongside
	if logEntry["session_id"] != sessionID {
		t.Errorf("Expected session_id='%s' (from WithSessionID), got %v", sessionID, logEntry["session_id"])
	}
	if logEntry["hook"] != "pre-push" {
		t.Errorf("Expected hook='pre-push', got %v", logEntry["hook"])
	}
	if logEntry["duration_ms"] != float64(150) {
		t.Errorf("Expected duration_ms=150, got %v", logEntry["duration_ms"])
	}
	if logEntry["success"] != true {
		t.Errorf("Expected success=true, got %v", logEntry["success"])
	}
}

func TestLogging_ConcurrentInitAndLog(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	initGitRepo(t, tmpDir)

	if _, err := Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	defer Close()

	const (
		logGoroutines   = 8
		initGoroutines  = 4
		closeGoroutines = 2
		iterations      = 200
	)

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range logGoroutines {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-start
			for j := range iterations {
				Info(context.Background(), "concurrent log", slog.Int("worker", worker), slog.Int("iteration", j))
			}
		}(i)
	}

	for range initGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				if _, err := Init(context.Background()); err != nil {
					t.Errorf("Init() error = %v", err)
					return
				}
			}
		}()
	}

	for range closeGoroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range iterations {
				Close()
			}
		}()
	}

	close(start)
	wg.Wait()
}

// TestLog_ResolvesLoggerFromContextFirst pins the resolution order the
// package-level helpers use: a logger in the context wins over the package
// global, so a caller holding an initialized context logs exactly where
// downstream packages that received the same logger by injection log.
func TestLog_ResolvesLoggerFromContextFirst(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	initGitRepo(t, tmpDir)

	if _, err := Init(context.Background()); err != nil {
		t.Fatalf("Init() error = %v", err)
	}

	// A logger distinguishable from the file-backed global.
	var buf bytes.Buffer
	ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	Warn(ctx, "routed by context")

	Close() // flush the global's file so the negative assertion is meaningful

	if !strings.Contains(buf.String(), "routed by context") {
		t.Errorf("line did not reach the context logger: %s", buf.String())
	}
	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}
	if strings.Contains(string(content), "routed by context") {
		t.Errorf("line also went to the package global; context must win: %s", content)
	}
}

// TestLog_CallerSuppliedSessionIDWins guards the collision between the session
// on the context and the one over a hundred call sites still pass by hand. slog
// does not dedupe attrs, so emitting both puts session_id in the JSON line
// twice; the caller's must be the one that survives.
func TestLog_CallerSuppliedSessionIDWins(t *testing.T) {
	t.Parallel()

	callerSessionID := "caller-supplied-session"
	tests := []struct {
		name  string
		attrs []any
	}{
		{"as slog.Attr", []any{slog.String("session_id", callerSessionID)}},
		{"as a loose key/value pair", []any{"session_id", callerSessionID}},
		{
			"after a loose pair whose value collides with the key",
			[]any{"resolved_from", "session_id", slog.String("session_id", callerSessionID)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
			ctx = WithSessionID(ctx, "context-session")
			Warn(ctx, "stamped once", tt.attrs...)

			line := buf.String()
			// Counting the key with its colon: the third case deliberately
			// passes "session_id" as a *value*, which must not be mistaken for
			// a second key here any more than it is by log() itself.
			if got := strings.Count(line, `"session_id":`); got != 1 {
				t.Errorf("session_id appears %d times, want 1: %s", got, line)
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("log output is not valid JSON: %v\nContent: %s", err, line)
			}
			if entry["session_id"] != callerSessionID {
				t.Errorf("session_id = %v, want the caller's %q", entry["session_id"], callerSessionID)
			}
		})
	}
}

// TestLog_ContextSessionIDIsScoped pins what the context value buys over the
// package global it replaced: re-stamping a derived context shadows the session
// for that scope only, leaving the parent's lines attributed to the parent.
func TestLog_ContextSessionIDIsScoped(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	outer := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	outer = WithSessionID(outer, "outer-session")
	inner := WithSessionID(outer, "inner-session")

	Warn(inner, "inner line")
	Warn(outer, "outer line")

	for _, want := range []struct{ msg, sessionID string }{
		{"inner line", "inner-session"},
		{"outer line", "outer-session"},
	} {
		var found bool
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if !strings.Contains(line, want.msg) {
				continue
			}
			found = true
			var entry map[string]any
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("log output is not valid JSON: %v\nContent: %s", err, line)
			}
			if entry["session_id"] != want.sessionID {
				t.Errorf("%q: session_id = %v, want %q", want.msg, entry["session_id"], want.sessionID)
			}
		}
		if !found {
			t.Errorf("%q missing from log output: %s", want.msg, buf.String())
		}
	}
}

// TestContextLogger_SurvivesReInit pins why loggers are bound to liveWriter
// rather than to the *bufio.Writer directly. Re-initializing with a resolved
// session ID is routine on the hook path (the root PersistentPreRunE inits
// first, then the hook re-inits), and Init flushes and drops the previous
// buffer. A logger captured before that must keep reaching the log file; bound
// to the old bufio.Writer it would write into an orphaned buffer nothing
// flushes, losing the line silently.
func TestContextLogger_SurvivesReInit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	initGitRepo(t, tmpDir)

	captured, err := Init(context.Background())
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	if captured == nil {
		t.Fatal("Init() returned no logger for a writable log directory")
	}

	if _, err := Init(context.Background()); err != nil {
		t.Fatalf("second Init() error = %v", err)
	}

	captured.Warn("written through the pre-reinit logger")
	Close()

	content, err := os.ReadFile(testLogFilePath(tmpDir))
	if err != nil {
		t.Fatalf("Failed to read log file: %v", err)
	}
	if !strings.Contains(string(content), "written through the pre-reinit logger") {
		t.Errorf("line written through the pre-reinit logger was lost: %s", content)
	}
}
