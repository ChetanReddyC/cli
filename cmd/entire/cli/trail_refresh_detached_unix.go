//go:build unix

package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// spawnDetachedTrailRefreshProcess starts `entire __refresh_trail_enablement`
// as a detached child so the trails-enablement network refresh can't add
// latency to the SessionStart hook that spawned it (#450). Mirrors the
// telemetry package's spawnDetachedAnalytics pattern.
func spawnDetachedTrailRefreshProcess(worktreeRoot string) {
	executable, err := os.Executable()
	if err != nil {
		return
	}

	cmd := exec.CommandContext(context.Background(), executable, "__refresh_trail_enablement")

	// Detach from parent process group so the subprocess survives the
	// SessionStart hook process exiting.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Run from the worktree root: the refresh needs to resolve the origin
	// remote and git-common-dir for cache storage.
	cmd.Dir = worktreeRoot

	// Inherit environment (auth config dir, API base URL overrides, etc).
	cmd.Env = os.Environ()

	// Discard stdout/stderr to prevent output leaking to the parent's terminal.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return
	}

	// Release the process so it can run independently.
	//nolint:errcheck // Best effort - refresh should continue regardless
	_ = cmd.Process.Release()
}
