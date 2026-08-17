package opencode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/entireio/cli/redact"

	"github.com/charmbracelet/x/ansi"
)

// openCodeCommandTimeout is the maximum time to wait for opencode CLI commands.
const openCodeCommandTimeout = 30 * time.Second

const openCodeErrorDetailMaxRunes = 300

type openCodeExportError struct {
	message string
	cause   error
}

func (e *openCodeExportError) Error() string { return e.message }
func (e *openCodeExportError) Unwrap() error { return e.cause }

// runOpenCodeExportToFile runs `opencode export <sessionID>` and redirects stdout
// to outputPath. This avoids pipe/stdout capture truncation bugs in some opencode versions.
//
// The export is staged in a sibling temp file and renamed over outputPath only on
// success, so a failed export leaves any existing transcript untouched. That file
// is often the ONLY local copy of the session: it is written by the turn-end hook
// and is not condensed into a checkpoint until the user commits. Both callers
// re-export over a possibly-populated path — PrepareTranscript on every turn end
// (cli/lifecycle.go) and FetchTranscript on attach — so writing in place would let
// a missing `opencode` binary, a rejected session, or a 30s timeout destroy the
// transcript it was asked to refresh.
//
// The temp name deliberately starts with "." and does not end in ".json" so
// concurrent .entire/tmp scanners (state files, pre-task files) skip it, mirroring
// makeInstallTmpPath in cli/plugin_store.go. os.Rename replaces the destination on
// both POSIX and Windows.
func runOpenCodeExportToFile(ctx context.Context, sessionID, outputPath string) (retErr error) {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	file, err := os.CreateTemp(filepath.Dir(outputPath), ".export-"+filepath.Base(outputPath)+"-*")
	if err != nil {
		return fmt.Errorf("failed to create export file: %w", err)
	}
	tmpPath := file.Name()
	closed := false
	closeFile := func() error {
		if closed {
			return nil
		}
		closed = true
		return file.Close()
	}
	defer func() {
		if closeErr := closeFile(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("failed to close export file: %w", closeErr)
		}
		// No-op once the rename below has moved the file into place.
		_ = os.Remove(tmpPath)
	}()

	cmd := exec.CommandContext(ctx, "opencode", "export", sessionID)
	cmd.Stdout = file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		return classifyOpenCodeExportError(ctx, runErr, stderr.String(), sessionID)
	}

	// Close before renaming: on Windows a rename of an open file fails, and the
	// close must be reported as an error here rather than after the file is live.
	if closeErr := closeFile(); closeErr != nil {
		return fmt.Errorf("failed to close export file: %w", closeErr)
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		return fmt.Errorf("failed to write export file: %w", err)
	}

	return nil
}

func classifyOpenCodeExportError(ctx context.Context, err error, stderr, sessionID string) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &openCodeExportError{
			message: fmt.Sprintf("OpenCode export timed out after %s. Try again.", openCodeCommandTimeout),
			cause:   context.DeadlineExceeded,
		}
	}

	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return &openCodeExportError{
			message: "OpenCode is not installed or is not available in PATH.",
			cause:   err,
		}
	}
	if errors.Is(err, os.ErrPermission) {
		return &openCodeExportError{
			message: "OpenCode could not be started because of insufficient permissions.",
			cause:   err,
		}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		detail := formatOpenCodeErrorDetail(stderr)
		if strings.HasPrefix(strings.ToLower(detail), "session not found") {
			return &openCodeExportError{
				message: fmt.Sprintf("OpenCode session %q was not found. Check the session ID and try again.", sessionID),
				cause:   err,
			}
		}
		if detail != "" {
			return &openCodeExportError{
				message: fmt.Sprintf("OpenCode could not export session %q: %s", sessionID, detail),
				cause:   err,
			}
		}
		return &openCodeExportError{
			message: fmt.Sprintf("OpenCode could not export session %q.", sessionID),
			cause:   err,
		}
	}

	return &openCodeExportError{message: "OpenCode export could not be started.", cause: err}
}

func formatOpenCodeErrorDetail(stderr string) string {
	for _, rawLine := range strings.Split(stderr, "\n") {
		line := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, ansi.Strip(rawLine))
		line = strings.TrimSpace(line)
		if len(line) < len("error:") || !strings.EqualFold(line[:len("error:")], "error:") {
			continue
		}

		detail := strings.Join(strings.Fields(redact.String(line[len("error:"):])), " ")
		runes := []rune(detail)
		if len(runes) > openCodeErrorDetailMaxRunes {
			return string(runes[:openCodeErrorDetailMaxRunes]) + "…"
		}
		return detail
	}
	return ""
}

// runOpenCodeSessionDelete runs `opencode session delete <sessionID>` to remove
// a session from OpenCode's database. Returns nil on success or if the session
// doesn't exist (nothing to delete).
func runOpenCodeSessionDelete(ctx context.Context, sessionID string) error {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "opencode", "session", "delete", sessionID)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("opencode session delete timed out after %s", openCodeCommandTimeout)
		}
		// "Session not found" means the session doesn't exist — nothing to delete.
		if strings.Contains(strings.ToLower(string(output)), "session not found") {
			return nil
		}
		return fmt.Errorf("opencode session delete failed: %w (output: %s)", err, string(output))
	}

	return nil
}

// runOpenCodeImport runs `opencode import <file>` to import a session into
// OpenCode's database. The import preserves the original session ID
// from the export file.
func runOpenCodeImport(ctx context.Context, exportFilePath string) error {
	ctx, cancel := context.WithTimeout(ctx, openCodeCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "opencode", "import", exportFilePath)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("opencode import timed out after %s", openCodeCommandTimeout)
		}
		return fmt.Errorf("opencode import failed: %w (output: %s)", err, string(output))
	}

	return nil
}
