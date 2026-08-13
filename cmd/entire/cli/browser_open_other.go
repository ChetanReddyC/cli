//go:build !windows

package cli

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// openBrowserPlatform launches the platform's URL opener with browserURL as a
// single argv element. Nothing here goes through a shell, so query separators
// (`&`) and percent-encoding in the URL are passed through literally — see
// browser_open_windows.go for what happens when a shell does get involved.
func openBrowserPlatform(ctx context.Context, browserURL string) error {
	var command string

	switch runtime.GOOS {
	case darwinGOOS:
		command = "open"
	case "linux":
		command = "xdg-open"
	default:
		return fmt.Errorf("unsupported platform %s", runtime.GOOS)
	}

	cmd := exec.CommandContext(ctx, command, browserURL)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start browser command %q: %w", command, err)
	}

	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release browser process: %w", err)
	}

	return nil
}
