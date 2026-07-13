package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/cli/cmd/entire/cli/paths"
	"github.com/entireio/cli/cmd/entire/cli/testutil"
)

// Regression for #1123: `entire enable --local` writes only
// .entire/settings.local.json, but the hook activation check
// (IsSetUpAndEnabled) only looked for .entire/settings.json, so hooks silently
// no-op'd. It must recognize a local-only setup.
func TestIsSetUpAndEnabled_LocalSettingsOnly(t *testing.T) {
	root := t.TempDir()
	testutil.InitRepo(t, root)
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
