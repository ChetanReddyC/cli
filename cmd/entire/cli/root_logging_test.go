package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
	"github.com/spf13/cobra"
)

// probeMarker is what the probe command writes through the logger it was
// handed, so the assertion can prove the line reached the log file rather than
// just that a logger existed.
const probeMarker = "root prerun injected this logger"

// markRepoSetUpForLogging writes the settings file that makes
// settings.IsSetUpAny true for the repo at cwd — the gate initRootLogging uses
// to keep a never-enabled repo free of an .entire/ directory.
func markRepoSetUpForLogging(t *testing.T) {
	t.Helper()

	if err := os.MkdirAll(paths.EntireDir, 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", paths.EntireDir, err)
	}
	if err := os.WriteFile(filepath.Join(paths.EntireDir, "settings.json"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatalf("write settings.json: %v", err)
	}
}

// executeThroughRoot runs args through a real root command, which is the only
// way to exercise the root PersistentPreRunE that initializes logging and the
// PersistentPostRun that flushes it. Output is discarded.
func executeThroughRoot(t *testing.T, args ...string) error {
	t.Helper()

	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	return root.ExecuteContext(context.Background())
}

// setUpRepoForRootLogging makes cwd a git repo that Entire has been set up in,
// which is what initRootLogging gates on.
func setUpRepoForRootLogging(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	markRepoSetUpForLogging(t)
	return dir
}

// runProbeUnder attaches a probe command under the command path in parents,
// executes it through the real root, and reports the logger its RunE observed
// in the context — writing probeMarker through it while the log file is still
// open, since the root PersistentPostRun flushes and closes on the way out.
//
// The probe is Hidden so that hook's parent-chain Hidden walk short-circuits
// before the telemetry and version-check calls, which would otherwise reach the
// network mid-test.
func runProbeUnder(t *testing.T, parents ...string) *logging.Logger {
	t.Helper()

	root := NewRootCmd()
	parent := root
	if len(parents) > 0 {
		found, _, err := root.Find(parents)
		if err != nil {
			t.Fatalf("root.Find(%v): %v", parents, err)
		}
		parent = found
	}

	var observed *logging.Logger
	probe := &cobra.Command{
		Use:    "__root_logging_probe",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			observed = logging.LoggerFromContext(cmd.Context())
			if observed != nil {
				observed.Slog().Warn(probeMarker)
			}
			return nil
		},
	}
	parent.AddCommand(probe)

	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(append(append([]string{}, parents...), probe.Use))
	if err := root.Execute(); err != nil {
		t.Fatalf("execute %v %s: %v", parents, probe.Use, err)
	}
	return observed
}

// TestRootPreRun_InjectsLoggerIntoCommandContext pins the contract every command
// now depends on instead of building a logger itself: after the root
// PersistentPreRunE, the executing command's context carries the initialized
// logger, and lines written through it land in .entire/logs/entire.log.
func TestRootPreRun_InjectsLoggerIntoCommandContext(t *testing.T) {
	dir := setUpRepoForRootLogging(t)

	if runProbeUnder(t) == nil {
		t.Fatal("LoggerFromContext() = nil after the root PersistentPreRunE ran")
	}

	content, err := os.ReadFile(filepath.Join(dir, paths.EntireDir, "logs", "entire.log"))
	if err != nil {
		t.Fatalf("read entire.log: %v", err)
	}
	if !bytes.Contains(content, []byte(probeMarker)) {
		t.Errorf("injected logger did not write to entire.log: %s", content)
	}
}

// TestRootPreRun_ReachesLeafUnderGroupWithOwnPreRun pins the dependency on
// cobra.EnableTraverseRunHooks. `checkpoint` — like `hooks`, `session`, and
// `agent` — defines its own PersistentPreRunE, and under cobra's default
// only-the-closest-hook behaviour that shadows the root hook entirely. The
// failure would be silent: no error, just every command under those groups
// logging to stderr again, which is exactly what routing redaction diagnostics
// into .entire/logs/ set out to stop.
func TestRootPreRun_ReachesLeafUnderGroupWithOwnPreRun(t *testing.T) {
	setUpRepoForRootLogging(t)

	for _, group := range []string{"checkpoint", "session", "agent"} {
		t.Run(group, func(t *testing.T) {
			if runProbeUnder(t, group) == nil {
				t.Errorf("LoggerFromContext() = nil for a leaf under %q, which defines its own PersistentPreRunE", group)
			}
		})
	}
}

// TestInitRootLogging_SkipsRepoThatNeverEnabledEntire pins the gate that keeps
// `enable` owning its own logger: building one CREATES .entire/logs/, so a repo
// that never set Entire up must come out of an unrelated command untouched
// rather than seeded with an untracked directory no gitignore entry covers yet.
func TestInitRootLogging_SkipsRepoThatNeverEnabledEntire(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)

	if runProbeUnder(t) != nil {
		t.Error("LoggerFromContext() must be nil in a repo that never enabled Entire")
	}
	if _, err := os.Stat(filepath.Join(dir, paths.EntireDir)); !os.IsNotExist(err) {
		t.Errorf(".entire/ must not be created in a repo that never enabled Entire (stat err = %v)", err)
	}
}

// TestExecutedCommandCarriesLoggerOnFailure pins the flush main.go depends on.
// Cobra returns out of Command.execute as soon as RunE errors, and as soon as
// required-flag validation fails — both before its PersistentPostRun loop, so
// root's flush never runs on either. With no package global left, the command
// ExecuteContextC hands back is the only route to the logger, and a failing
// hook's diagnostics are exactly the lines worth not losing in the 8KB buffer.
//
// Errors raised before any pre-run (unknown flag or subcommand, bad args) return
// a command carrying no logger, which is harmless: none was built, so there is
// nothing buffered to lose.
func TestExecutedCommandCarriesLoggerOnFailure(t *testing.T) {
	tests := []struct {
		name          string
		requireFlag   bool
		runE          func(cmd *cobra.Command, args []string) error
		wantErrSubstr string
	}{
		{
			name: "RunE returns an error",
			runE: func(cmd *cobra.Command, _ []string) error {
				logging.Warn(cmd.Context(), probeMarker)
				return errors.New("probe failed")
			},
			wantErrSubstr: "probe failed",
		},
		{
			// Validated after the pre-runs, so a logger exists and has buffered
			// content by the time cobra bails out.
			name:        "required flag missing",
			requireFlag: true,
			runE: func(*cobra.Command, []string) error {
				return errors.New("RunE must not be reached")
			},
			wantErrSubstr: `required flag(s) "must" not set`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := setUpRepoForRootLogging(t)

			root := NewRootCmd()
			probe := &cobra.Command{
				Use:    "__root_logging_failure_probe",
				Hidden: true,
				RunE:   tt.runE,
			}
			if tt.requireFlag {
				// Log from the pre-run instead: RunE is never reached here.
				probe.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
					logging.Warn(cmd.Context(), probeMarker)
				}
				probe.Flags().String("must", "", "a required flag")
				if err := probe.MarkFlagRequired("must"); err != nil {
					t.Fatalf("MarkFlagRequired: %v", err)
				}
			}
			root.AddCommand(probe)
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{probe.Use})

			executed, err := root.ExecuteContextC(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErrSubstr)
			}
			if executed == nil {
				t.Fatal("ExecuteContextC returned no command; main.go would have nothing to close")
			}

			logFile := filepath.Join(dir, paths.EntireDir, "logs", "entire.log")
			buffered, readErr := os.ReadFile(logFile)
			if readErr != nil {
				t.Fatalf("read entire.log: %v", readErr)
			}
			if bytes.Contains(buffered, []byte(probeMarker)) {
				t.Skip("line reached the file without a flush; this test can no longer prove main.go's close is what flushes it")
			}

			// Exactly what main.go does after ExecuteContextC.
			if closeErr := logging.LoggerFromContext(executed.Context()).Close(); closeErr != nil {
				t.Fatalf("Close() error = %v", closeErr)
			}

			flushed, readErr := os.ReadFile(logFile)
			if readErr != nil {
				t.Fatalf("read entire.log after close: %v", readErr)
			}
			if !bytes.Contains(flushed, []byte(probeMarker)) {
				t.Errorf("line logged before the failure was lost: %s", flushed)
			}
		})
	}
}
