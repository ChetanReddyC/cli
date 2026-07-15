package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	agentpkg "github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/stretchr/testify/require"
)

// setupTestEnv creates a temp dir, sets CWD and CODEX_HOME for test isolation.
// Cannot be parallel (uses t.Chdir and t.Setenv which are process-global).
func setupTestEnv(t *testing.T) string {
	t.Helper()
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	t.Setenv("CODEX_HOME", filepath.Join(tempDir, ".codex-home"))
	return tempDir
}

func TestInstallHooks_CreatesConfig(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count) // SessionStart, UserPromptSubmit, Stop, PostToolUse

	// Verify hooks.json was created in the repo
	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)

	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")

	// Hooks are enabled by default in Codex, so no .codex/config.toml is
	// written. A TOML file there is actively harmful when the repo lives
	// inside <CODEX_HOME>/agents, where Codex's agent-role scanner rejects
	// it at startup (entireio/cli#842).
	projectConfig := filepath.Join(tempDir, ".codex", configFileName)
	_, err = os.Stat(projectConfig)
	require.True(t, os.IsNotExist(err), "install must not create .codex/config.toml")
}

func TestInstallHooks_WindowsWrapperProbeSuccessKeepsWrappedCommands(t *testing.T) {
	tempDir := setupTestEnv(t)
	withCodexHookEnvironment(t, "windows", true)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)

	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")
}

func TestInstallHooks_WindowsWrapperProbeFailureUsesWindowsCommands(t *testing.T) {
	tempDir := setupTestEnv(t)
	withCodexHookEnvironment(t, "windows", false)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)

	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapWindowsProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")
	require.NotContains(t, string(data), "sh -c")
	require.NotContains(t, string(data), "command -v entire")
	require.Contains(t, string(data), "where.exe entire")
}

func TestInstallHooks_WindowsWrapperProbeFailureMigratesToWindowsCommands(t *testing.T) {
	tempDir := setupTestEnv(t)
	wrapperWorks := true
	withCodexHookEnvironmentFunc(t, "windows", func(context.Context, string) bool {
		return wrapperWorks
	})

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	wrapperWorks = false
	count, err = ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)

	var hooksFile HooksFile
	require.NoError(t, json.Unmarshal(data, &hooksFile))

	assertHookCommand(t, hooksFile.Hooks.SessionStart, agentpkg.WrapWindowsProductionJSONWarningHookCommand("entire hooks codex session-start", agentpkg.WarningFormatSingleLine), "SessionStart")
	assertHookCommand(t, hooksFile.Hooks.UserPromptSubmit, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex user-prompt-submit"), "UserPromptSubmit")
	assertHookCommand(t, hooksFile.Hooks.Stop, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex stop"), "Stop")
	assertHookCommand(t, hooksFile.Hooks.PostToolUse, agentpkg.WrapWindowsProductionSilentHookCommand("entire hooks codex post-tool-use"), "PostToolUse")
	require.NotContains(t, string(data), "sh -c")
	require.NotContains(t, string(data), "command -v entire")
	require.Contains(t, string(data), "where.exe entire")
}

func TestInstallHooks_Idempotent(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	count1, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count1)

	count2, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 0, count2)
}

func TestInstallHooks_LocalDev(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), true, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	hooksPath := filepath.Join(tempDir, ".codex", HooksFileName)
	data, err := os.ReadFile(hooksPath)
	require.NoError(t, err)
	require.Contains(t, string(data), `\"$(git rev-parse --show-toplevel)\"/scripts/entire-dev hooks codex session-start`)
	require.Contains(t, string(data), `\"$(git rev-parse --show-toplevel)\"/scripts/entire-dev hooks codex post-tool-use`)
}

func TestInstallHooks_Force(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	count, err := ag.InstallHooks(context.Background(), false, true)
	require.NoError(t, err)
	require.Equal(t, 4, count)
}

func TestUninstallHooks(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}

	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	err = ag.UninstallHooks(context.Background())
	require.NoError(t, err)

	require.False(t, ag.AreHooksInstalled(context.Background()))
}

func TestUninstallHooks_PreservesUserHookContainingEntireSubstring(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"Stop": [
				{
					"matcher": null,
					"hooks": [
						{"type": "command", "command": "echo \"the entire workflow finished\""}
					]
				}
			]
		}
	}`
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	err = ag.UninstallHooks(context.Background())
	require.NoError(t, err)

	data, readErr := os.ReadFile(hooksPath)
	require.NoError(t, readErr)
	require.Contains(t, string(data), `echo \"the entire workflow finished\"`)
	require.NotContains(t, string(data), "entire hooks codex stop")
}

func TestAreHooksInstalled_NoFile(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}
	require.False(t, ag.AreHooksInstalled(context.Background()))
}

func TestAreHooksInstalled_WithHooks(t *testing.T) {
	setupTestEnv(t)

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	require.True(t, ag.AreHooksInstalled(context.Background()))
}

func TestAreHooksInstalled_PartialHooks(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, HooksFileName), []byte(`{
		"hooks": {
			"Stop": [
				{
					"matcher": null,
					"hooks": [
						{"type": "command", "command": "entire hooks codex stop", "timeout": 30}
					]
				}
			]
		}
	}`), 0o600))

	ag := &CodexAgent{}
	require.False(t, ag.AreHooksInstalled(context.Background()))
}

func TestInstallHooks_PreservesExistingHooksJSON(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"PreToolUse": [
				{
					"matcher": "^Bash$",
					"hooks": [
						{"type": "command", "command": "my-custom-hook"}
					]
				}
			]
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(codexDir, HooksFileName), []byte(existingConfig), 0o600))

	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(codexDir, HooksFileName))
	require.NoError(t, err)
	require.Contains(t, string(data), "my-custom-hook")
	require.Contains(t, string(data), "entire hooks codex stop")
}

func TestInstallHooks_ErrorsOnMalformedManagedHook(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"SessionStart": {"not": "an array"},
			"PreToolUse": [
				{
					"matcher": "^Bash$",
					"hooks": [
						{"type": "command", "command": "my-custom-hook"}
					]
				}
			]
		}
	}`
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to parse SessionStart hooks")

	data, readErr := os.ReadFile(hooksPath)
	require.NoError(t, readErr)
	require.JSONEq(t, existingConfig, string(data))
}

func TestUninstallHooks_ErrorsOnMalformedManagedHook(t *testing.T) {
	tempDir := setupTestEnv(t)

	codexDir := filepath.Join(tempDir, ".codex")
	require.NoError(t, os.MkdirAll(codexDir, 0o750))
	existingConfig := `{
		"hooks": {
			"Stop": {"not": "an array"}
		}
	}`
	hooksPath := filepath.Join(codexDir, HooksFileName)
	require.NoError(t, os.WriteFile(hooksPath, []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	err := ag.UninstallHooks(context.Background())
	require.Error(t, err)
	require.ErrorContains(t, err, "failed to parse Stop hooks")

	data, readErr := os.ReadFile(hooksPath)
	require.NoError(t, readErr)
	require.JSONEq(t, existingConfig, string(data))
}

func TestInstallHooks_DoesNotModifyUserConfig(t *testing.T) {
	setupTestEnv(t)
	codexHome := os.Getenv("CODEX_HOME")

	require.NoError(t, os.MkdirAll(codexHome, 0o750))
	existingConfig := "model = \"gpt-4.1\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, configFileName), []byte(existingConfig), 0o600))

	ag := &CodexAgent{}
	_, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)

	configData, err := os.ReadFile(filepath.Join(codexHome, configFileName))
	require.NoError(t, err)
	require.Contains(t, string(configData), "model = \"gpt-4.1\"")
	require.NotContains(t, string(configData), `trust_level = "trusted"`)
}

// TestInstallHooks_CleansUpStaleFeatureConfig pins the self-heal: a
// project-local .codex/config.toml containing only the feature-flag content
// older entire versions wrote (either the current `hooks = true` form or the
// legacy `codex_hooks = true` form) is removed on the next install. Hooks
// are enabled by default in Codex, and a leftover TOML file under
// <CODEX_HOME>/agents triggers a "malformed agent role definition" warning
// at every Codex startup (entireio/cli#842).
func TestInstallHooks_CleansUpStaleFeatureConfig(t *testing.T) {
	staleContents := map[string]string{
		"current form": "[features]\nhooks = true\n",
		"legacy form":  "[features]\ncodex_hooks = true\n",
		"both forms":   "[features]\ncodex_hooks = true\nhooks = true\n",
		"extra blanks": "\n[features]\n\nhooks = true\n\n",
	}
	for name, content := range staleContents {
		t.Run(name, func(t *testing.T) {
			tempDir := setupTestEnv(t)

			codexDir := filepath.Join(tempDir, ".codex")
			require.NoError(t, os.MkdirAll(codexDir, 0o750))
			configPath := filepath.Join(codexDir, configFileName)
			require.NoError(t, os.WriteFile(configPath, []byte(content), 0o600))

			ag := &CodexAgent{}
			_, err := ag.InstallHooks(context.Background(), false, false)
			require.NoError(t, err)

			_, err = os.Stat(configPath)
			require.True(t, os.IsNotExist(err), "stale entire-managed config.toml must be removed")
		})
	}
}

// TestInstallHooks_CleansUpStaleFeatureConfig_WhenHooksAlreadyInstalled pins
// that the self-heal also runs on a re-enable where hooks.json is already
// up to date (the count == 0 path), so upgrading entire fixes the leftover
// without requiring a force reinstall.
func TestInstallHooks_CleansUpStaleFeatureConfig_WhenHooksAlreadyInstalled(t *testing.T) {
	tempDir := setupTestEnv(t)

	ag := &CodexAgent{}
	count, err := ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 4, count)

	configPath := filepath.Join(tempDir, ".codex", configFileName)
	require.NoError(t, os.WriteFile(configPath, []byte("[features]\nhooks = true\n"), 0o600))

	count, err = ag.InstallHooks(context.Background(), false, false)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	_, err = os.Stat(configPath)
	require.True(t, os.IsNotExist(err), "stale config.toml must be removed even when hooks.json is already current")
}

// TestInstallHooks_PreservesUserLocalConfig pins that cleanup never deletes a
// .codex/config.toml carrying anything beyond the exact feature-flag content
// this package used to write — a user's own settings, comments, or lines that
// merely contain our tokens as substrings (`webhooks = true`) make the file
// unmanaged and it is left byte-identical.
func TestInstallHooks_PreservesUserLocalConfig(t *testing.T) {
	userContents := map[string]string{
		"user setting alongside flag": "[features]\nhooks = true\nsandbox = \"workspace-write\"\n",
		"comment":                     "# my codex config\n[features]\nhooks = true\n",
		"substring lookalike":         "[features]\nwebhooks = true\n",
		"unrelated file":              "model = \"gpt-4.1\"\n",
	}
	for name, content := range userContents {
		t.Run(name, func(t *testing.T) {
			tempDir := setupTestEnv(t)

			codexDir := filepath.Join(tempDir, ".codex")
			require.NoError(t, os.MkdirAll(codexDir, 0o750))
			configPath := filepath.Join(codexDir, configFileName)
			require.NoError(t, os.WriteFile(configPath, []byte(content), 0o600))

			ag := &CodexAgent{}
			_, err := ag.InstallHooks(context.Background(), false, false)
			require.NoError(t, err)

			data, err := os.ReadFile(configPath)
			require.NoError(t, err)
			require.Equal(t, content, string(data), "config.toml with user content must be left untouched")
		})
	}
}

// assertHookCommand verifies that one of the hook entries in groups contains the expected command.
func assertHookCommand(t *testing.T, groups []MatcherGroup, expectedCmd, label string) {
	t.Helper()
	for _, g := range groups {
		for _, h := range g.Hooks {
			if h.Command == expectedCmd {
				return
			}
		}
	}
	t.Errorf("%s: expected hook command not found: %s", label, expectedCmd)
}

func withCodexHookEnvironment(t *testing.T, goos string, wrapperWorks bool) {
	t.Helper()
	withCodexHookEnvironmentFunc(t, goos, func(context.Context, string) bool {
		return wrapperWorks
	})
}

func withCodexHookEnvironmentFunc(t *testing.T, goos string, wrapperWorks func(context.Context, string) bool) {
	t.Helper()
	t.Cleanup(agentpkg.SetWindowsHookProbeForTesting(goos, wrapperWorks))
}
