package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
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
func runProbeUnder(t *testing.T, parents ...string) *slog.Logger {
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

	var observed *slog.Logger
	probe := &cobra.Command{
		Use:    "__root_logging_probe",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			observed = logging.LoggerFromContext(cmd.Context())
			if observed != nil {
				observed.Warn(probeMarker)
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
// now depends on instead of calling logging.Init itself: after the root
// PersistentPreRunE, the executing command's context carries the initialized
// logger, and lines written through it land in .entire/logs/entire.log.
func TestRootPreRun_InjectsLoggerIntoCommandContext(t *testing.T) {
	dir := setUpRepoForRootLogging(t)
	defer logging.Close()

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
	defer logging.Close()

	for _, group := range []string{"checkpoint", "session", "agent"} {
		t.Run(group, func(t *testing.T) {
			if runProbeUnder(t, group) == nil {
				t.Errorf("LoggerFromContext() = nil for a leaf under %q, which defines its own PersistentPreRunE", group)
			}
		})
	}
}

// TestInitRootLogging_SkipsRepoThatNeverEnabledEntire pins the gate that keeps
// `enable` owning its own logging.Init: Init CREATES .entire/logs/, so a repo
// that never set Entire up must come out of an unrelated command untouched
// rather than seeded with an untracked directory no gitignore entry covers yet.
func TestInitRootLogging_SkipsRepoThatNeverEnabledEntire(t *testing.T) {
	dir := t.TempDir()
	testutil.InitRepo(t, dir)
	testutil.WriteFile(t, dir, "f.txt", "init")
	testutil.GitAdd(t, dir, "f.txt")
	testutil.GitCommit(t, dir, "init")
	t.Chdir(dir)
	defer logging.Close()

	if runProbeUnder(t) != nil {
		t.Error("LoggerFromContext() must be nil in a repo that never enabled Entire")
	}
	if _, err := os.Stat(filepath.Join(dir, paths.EntireDir)); !os.IsNotExist(err) {
		t.Errorf(".entire/ must not be created in a repo that never enabled Entire (stat err = %v)", err)
	}
}
