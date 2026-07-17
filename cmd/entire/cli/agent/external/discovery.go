package external

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/agent/types"
	"github.com/entireio/cli/cmd/entire/cli/logging"
	"github.com/entireio/cli/cmd/entire/cli/settings"
)

const (
	binaryPrefix = "entire-agent-"
	osWindows    = "windows"
)

// discoveryTimeout caps the total time spent scanning $PATH for external agents.
const discoveryTimeout = 10 * time.Second

// DiscoverAndRegister scans $PATH for executables matching "entire-agent-<name>",
// calls their "info" subcommand, and registers them in the agent registry.
// Binaries whose name conflicts with an already-registered agent are skipped.
// Errors during discovery are logged but do not prevent other agents from loading.
// Discovery is skipped when the external_agents setting is not enabled.
func DiscoverAndRegister(ctx context.Context) {
	if !settings.IsExternalAgentsEnabled(ctx) {
		logging.Debug(ctx, "external agent discovery disabled (external_agents not enabled in settings)")
		return
	}
	discoverAndRegister(ctx)
}

// DiscoverAndRegisterAlways is like DiscoverAndRegister but bypasses the
// external_agents settings check. Use this in interactive setup flows where
// the user explicitly chooses agents.
func DiscoverAndRegisterAlways(ctx context.Context) {
	discoverAndRegister(ctx)
}

// DiscoverAndRegisterNamedAlways discovers and registers only the external
// agent binary matching name. It bypasses the external_agents setting for
// explicit, one-invocation selections without executing unrelated plugins.
func DiscoverAndRegisterNamedAlways(ctx context.Context, name types.AgentName) {
	discoverAndRegisterNamed(ctx, name, discoveryTimeout)
}

func discoverAndRegisterNamed(ctx context.Context, name types.AgentName, timeout time.Duration) {
	if name == "" {
		return
	}
	if _, err := agent.Get(name); err == nil {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	binPath, err := exec.LookPath(binaryPrefix + string(name))
	if err != nil {
		return
	}
	registerExternalAgent(ctx, binPath, name)
}

// discoverAndRegister contains the shared scanning logic for external agent discovery.
func discoverAndRegister(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return
	}

	// Collect already-registered names to avoid conflicts.
	registered := make(map[types.AgentName]bool)
	for _, name := range agent.List() {
		registered[name] = true
	}

	seen := make(map[string]bool) // deduplicate binaries across PATH dirs
	for _, dir := range filepath.SplitList(pathEnv) {
		if ctx.Err() != nil {
			logging.Debug(ctx, "external agent discovery timed out")
			return
		}

		matches, err := filepath.Glob(filepath.Join(dir, binaryPrefix+"*"))
		if err != nil {
			continue // skip unreadable directories
		}
		for _, binPath := range matches {
			if ctx.Err() != nil {
				logging.Debug(ctx, "external agent discovery timed out")
				return
			}

			name := filepath.Base(binPath)
			if seen[name] {
				continue
			}
			seen[name] = true

			// Strip Windows executable extensions (.exe, .bat) before deriving agent name.
			// On Unix, binaries have no extension, so this is a no-op.
			cleanName := StripExeExt(name)
			agentName := types.AgentName(strings.TrimPrefix(cleanName, binaryPrefix))
			if registered[agentName] {
				logging.Debug(ctx, "skipping external agent (name conflict with built-in)",
					slog.String("binary", name),
					slog.String("agent", string(agentName)))
				continue
			}

			if registerExternalAgent(ctx, binPath, agentName) {
				registered[agentName] = true
			}
		}
	}
}

func registerExternalAgent(ctx context.Context, binPath string, name types.AgentName) bool {
	finfo, err := os.Stat(binPath) //nolint:gosec // PATH entries are trusted
	if err != nil || finfo.IsDir() {
		return false
	}
	// Check executable bit (on Unix; Windows doesn't set execute bits).
	if runtime.GOOS != osWindows && finfo.Mode()&0o111 == 0 {
		return false
	}

	ea, err := New(ctx, binPath)
	if err != nil {
		logging.Debug(ctx, "skipping external agent (info failed)",
			slog.String("binary", binPath),
			slog.String("error", err.Error()))
		return false
	}

	wrapped, err := Wrap(ea)
	if err != nil {
		logging.Debug(ctx, "skipping external agent (wrap failed)",
			slog.String("binary", binPath),
			slog.String("error", err.Error()))
		return false
	}
	agent.Register(name, func() agent.Agent {
		return wrapped
	})

	logging.Debug(ctx, "registered external agent",
		slog.String("name", string(name)),
		slog.String("type", string(ea.Type())),
		slog.String("binary", binPath))
	return true
}

// StripExeExt removes Windows executable extensions (.exe, .bat, .cmd, .com)
// from a file name so that the derived name matches on all platforms. On Unix
// this is effectively a no-op because binaries have no extension.
//
// .com is included because Windows PATHEXT defaults to ".COM;.EXE;.BAT;.CMD;…",
// so exec.LookPath can resolve a `.com` next to a `.exe`. Without stripping
// it, a managed-plugin or agent-binary installer would treat foo.exe and
// foo.com as distinct names while PATHEXT silently picks one.
func StripExeExt(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".exe", ".bat", ".cmd", ".com":
		return strings.TrimSuffix(name, filepath.Ext(name))
	}
	return name
}
