//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestIssue1140_ReenableAfterProjectDisable_EnablesProjectFile is a full-flow
// reproduction of #1140: after `entire disable --project`, running
// `entire enable --checkpoint-remote ...` with no --project/--local reported
// success but wrote the enabled flag to .entire/settings.local.json, leaving the
// project .entire/settings.json the user disabled still enabled=false. `entire
// status` then still showed disabled.
//
// This drives the real entire binary end-to-end — enable, disable --project,
// then a setup-flag re-enable — and asserts the PROJECT settings.json (the file
// the user actually disabled) is enabled again.
func TestIssue1140_ReenableAfterProjectDisable_EnablesProjectFile(t *testing.T) {
	t.Parallel()
	env := NewTestEnv(t)
	defer env.Cleanup()

	env.InitRepo()

	// First-time setup via the real binary; a plain enable writes the project
	// .entire/settings.json.
	env.RunCLI("enable", "--agent", "claude-code", "--telemetry=false")
	assertProjectSettingsEnabled(t, env, true)

	// Disable at the project scope → settings.json enabled=false.
	env.RunCLI("disable", "--project")
	assertProjectSettingsEnabled(t, env, false)

	// Re-enable with a setup flag but WITHOUT --project/--local. Pre-fix the
	// enabled flag landed in settings.local.json, so the project file the user
	// disabled stayed enabled=false (#1140).
	env.RunCLI("enable", "--checkpoint-remote", "github:org/repo", "--skip-push-sessions", "--telemetry=false")

	assertProjectSettingsEnabled(t, env, true)
}

// assertProjectSettingsEnabled reads .entire/settings.json (the project scope,
// never settings.local.json) and asserts its enabled flag matches want.
func assertProjectSettingsEnabled(t *testing.T, env *TestEnv, want bool) {
	t.Helper()
	settingsPath := filepath.Join(env.RepoDir, ".entire", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read .entire/settings.json: %v", err)
	}
	var s struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse .entire/settings.json: %v\ncontent: %s", err, data)
	}
	if s.Enabled != want {
		t.Fatalf("project settings.json enabled=%v, want %v — enabled flag written to the wrong scope (#1140)\ncontent: %s",
			s.Enabled, want, data)
	}
}
