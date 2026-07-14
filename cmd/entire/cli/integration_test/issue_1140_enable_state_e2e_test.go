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
// project .entire/settings.json the user disabled still enabled=false.
//
// Because settings.local.json (enabled:true) overrides settings.json in the
// merged view that both `entire status` and IsEnabled read, status actually
// reported ENABLED — the effective state was correct and only the committed
// file was stale. That still bites anyone without the local file (a fresh
// clone, a teammate) and leaves the committed source of truth wrong.
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

	// And no local override may contradict it: settings.local.json must be
	// absent or itself enabled:true, so the merged view can't silently flip
	// back to disabled by accident of the flow.
	assertLocalSettingsAbsentOrEnabled(t, env)
}

// assertLocalSettingsAbsentOrEnabled asserts that .entire/settings.local.json,
// if present, does not carry an enabled:false override that would mask the
// committed project scope.
func assertLocalSettingsAbsentOrEnabled(t *testing.T, env *TestEnv) {
	t.Helper()
	localPath := filepath.Join(env.RepoDir, ".entire", "settings.local.json")
	data, err := os.ReadFile(localPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatalf("read .entire/settings.local.json: %v", err)
	}
	var s struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse .entire/settings.local.json: %v\ncontent: %s", err, data)
	}
	if s.Enabled != nil && !*s.Enabled {
		t.Fatalf("settings.local.json carries enabled:false, which would mask the re-enabled project scope\ncontent: %s", data)
	}
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
