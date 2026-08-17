package opencode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClassifyOpenCodeExportError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		stderr    string
		sessionID string
		want      string
	}{
		{
			name:      "missing executable",
			err:       &exec.Error{Name: "opencode", Err: exec.ErrNotFound},
			sessionID: "ses_missing_binary",
			want:      "OpenCode is not installed or is not available in PATH.",
		},
		{
			name:      "permission denied",
			err:       &os.PathError{Op: "fork/exec", Path: "/private/opencode", Err: os.ErrPermission},
			sessionID: "ses_denied",
			want:      "OpenCode could not be started because of insufficient permissions.",
		},
		{
			name:      "missing session",
			err:       &exec.ExitError{},
			stderr:    "Exporting session: ses_missing\nError: Session not found: ses_missing\n",
			sessionID: "ses_missing",
			want:      `OpenCode session "ses_missing" was not found. Check the session ID and try again.`,
		},
		{
			name:      "useful stderr",
			err:       &exec.ExitError{},
			stderr:    "Exporting session\n\x1b[31mError: Export was rejected by OpenCode\x1b[0m\nDB_PASSWORD=not-a-real-secret\n",
			sessionID: "ses_rejected",
			want:      `OpenCode could not export session "ses_rejected": Export was rejected by OpenCode`,
		},
		{
			name:      "unstructured stderr",
			err:       &exec.ExitError{},
			stderr:    "internal stack trace\nDB_PASSWORD=not-a-real-secret\n",
			sessionID: "ses_opaque",
			want:      `OpenCode could not export session "ses_opaque".`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := classifyOpenCodeExportError(context.Background(), tt.err, tt.stderr, tt.sessionID)
			if got.Error() != tt.want {
				t.Fatalf("classifyOpenCodeExportError error = %q, want %q", got, tt.want)
			}
			if !errors.Is(got, tt.err) {
				t.Fatal("classified error does not retain its cause")
			}
		})
	}
}

func TestClassifyOpenCodeExportError_Timeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()

	err := classifyOpenCodeExportError(ctx, errors.New("signal: killed"), "", "ses_timeout")
	want := "OpenCode export timed out after 30s. Try again."
	if err.Error() != want {
		t.Fatalf("classifyOpenCodeExportError error = %q, want %q", err, want)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("timeout error does not retain context.DeadlineExceeded")
	}
}

// TestRunOpenCodeExportToFile_FailedExportPreservesExistingFile is a regression
// test for a failed export destroying the transcript it was asked to refresh.
// The hook-cached export under .entire/tmp is the only local copy of the session
// until the user commits, and both callers (PrepareTranscript on every turn end,
// FetchTranscript on attach) re-export over it.
func TestRunOpenCodeExportToFile_FailedExportPreservesExistingFile(t *testing.T) {
	// No t.Parallel: t.Setenv.
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "ses_cached.json")
	const cached = `{"info":{"id":"ses_cached"},"messages":[]}`
	if err := os.WriteFile(outputPath, []byte(cached), 0o600); err != nil {
		t.Fatal(err)
	}

	// Empty PATH makes the export fail deterministically without an opencode binary.
	t.Setenv("PATH", "")

	err := runOpenCodeExportToFile(context.Background(), "ses_cached", outputPath)
	if err == nil {
		t.Fatal("expected export to fail with no opencode on PATH")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("runOpenCodeExportToFile error = %v, want exec.ErrNotFound", err)
	}

	got, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		t.Fatalf("cached transcript was destroyed by the failed export: %v", readErr)
	}
	if string(got) != cached {
		t.Fatalf("cached transcript = %q, want it left untouched (%q)", string(got), cached)
	}
	assertNoExportTempFiles(t, dir)
}

func TestRunOpenCodeExportToFile_ReplacesExistingFileOnSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub opencode is a shell script")
	}
	// No t.Parallel: t.Setenv.
	dir := t.TempDir()
	outputPath := filepath.Join(dir, "ses_ok.json")
	if err := os.WriteFile(outputPath, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	const export = `{"info":{"id":"ses_ok"},"messages":[]}`
	stubDir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '" + export + "'\n"
	if err := os.WriteFile(filepath.Join(stubDir, "opencode"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir)

	if err := runOpenCodeExportToFile(context.Background(), "ses_ok", outputPath); err != nil {
		t.Fatalf("runOpenCodeExportToFile failed: %v", err)
	}

	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != export {
		t.Fatalf("exported transcript = %q, want %q", string(got), export)
	}
	assertNoExportTempFiles(t, dir)
}

// assertNoExportTempFiles fails if a staged export was left behind in dir.
func assertNoExportTempFiles(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".export-") {
			t.Errorf("staged export left behind: %s", entry.Name())
		}
	}
}

func TestFormatOpenCodeErrorDetail_Truncates(t *testing.T) {
	t.Parallel()

	detail := formatOpenCodeErrorDetail("Error: " + strings.Repeat("a", openCodeErrorDetailMaxRunes+1))
	want := strings.Repeat("a", openCodeErrorDetailMaxRunes) + "…"
	if detail != want {
		t.Fatalf("formatOpenCodeErrorDetail = %q, want %q", detail, want)
	}
}
