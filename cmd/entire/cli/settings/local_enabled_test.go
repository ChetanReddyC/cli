package settings

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
)

// Regression for #1123: `entire enable --local` writes only
// .entire/settings.local.json, but the hook activation check
// (IsSetUpAndEnabled) only looked for .entire/settings.json, so hooks silently
// no-op'd. It must recognize a local-only setup.
func TestIsSetUpAndEnabled_LocalSettingsOnly(t *testing.T) {
	root := t.TempDir()
	cmd := exec.CommandContext(context.Background(), "git", "init")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	entireDir := filepath.Join(root, ".entire")
	if err := os.MkdirAll(entireDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the local settings file exists (no settings.json), enabled.
	if err := os.WriteFile(filepath.Join(entireDir, "settings.local.json"), []byte(`{"enabled":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)
	paths.ClearWorktreeRootCache()

	if IsSetUp(context.Background()) {
		t.Fatal("precondition: IsSetUp should be false with only settings.local.json")
	}
	if !IsSetUpAndEnabled(context.Background()) {
		t.Fatal("IsSetUpAndEnabled should be true when only settings.local.json exists and is enabled (#1123)")
	}
}
