package settings

import (
	"context"
	"testing"
)

// These tests use setupSettingsDir (t.Chdir) and t.Setenv, both process-global,
// so they cannot run in parallel.

func TestIsSemanticSearchV4Enabled_DefaultsFalse(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true}`, "")
	if IsSemanticSearchV4Enabled(context.Background()) {
		t.Error("semantic-search-v4 should be off by default")
	}
}

func TestIsSemanticSearchV4Enabled_FileEnabled(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true, "semantic_search_v4": true}`, "")
	if !IsSemanticSearchV4Enabled(context.Background()) {
		t.Error("semantic_search_v4: true should enable the v4 path")
	}
}

func TestIsSemanticSearchV4Enabled_EnvOverride(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true}`, "")
	t.Setenv("ENTIRE_SEMANTIC_SEARCH_V4", "1")
	if !IsSemanticSearchV4Enabled(context.Background()) {
		t.Error("ENTIRE_SEMANTIC_SEARCH_V4=1 should enable the v4 path regardless of settings")
	}
}

func TestIsSemanticSearchV4Enabled_EnvOverrideTrue(t *testing.T) {
	setupSettingsDir(t, `{"enabled": true}`, "")
	t.Setenv("ENTIRE_SEMANTIC_SEARCH_V4", "true")
	if !IsSemanticSearchV4Enabled(context.Background()) {
		t.Error("ENTIRE_SEMANTIC_SEARCH_V4=true should enable the v4 path")
	}
}

func TestIsSemanticSearchV4Enabled_LocalFileEnables(t *testing.T) {
	// The gitignored settings.local.json is the natural place to opt into a
	// rollout feature; the merge path must honor it.
	setupSettingsDir(t, `{"enabled": true}`, `{"semantic_search_v4": true}`)
	if !IsSemanticSearchV4Enabled(context.Background()) {
		t.Error("semantic_search_v4 in settings.local.json must enable the v4 path")
	}
}

// TestSemanticSearchV4_JSONTag guards the JSON field name. LoadFromBytes uses
// DisallowUnknownFields, so a wrong tag fails to parse.
func TestSemanticSearchV4_JSONTag(t *testing.T) {
	t.Parallel()
	s, err := LoadFromBytes([]byte(`{"enabled": true, "semantic_search_v4": true}`))
	if err != nil {
		t.Fatalf("LoadFromBytes() error = %v", err)
	}
	if !s.SemanticSearchV4 {
		t.Errorf("semantic_search_v4 did not parse into EntireSettings.SemanticSearchV4")
	}
}
