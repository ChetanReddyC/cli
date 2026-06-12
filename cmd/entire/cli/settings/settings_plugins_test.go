package settings

import (
	"strings"
	"testing"
	"time"
)

func TestPluginSettings_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      *PluginSettings
		wantErr string
	}{
		{name: "nil", in: nil},
		{name: "empty", in: &PluginSettings{}},
		{name: "https", in: &PluginSettings{IndexURL: "https://github.com/entireio/plugin-index"}},
		{name: "ssh", in: &PluginSettings{IndexURL: "ssh://git@example.com/idx.git"}},
		{name: "scp-like", in: &PluginSettings{IndexURL: "git@github.com:entireio/plugin-index.git"}},
		{name: "file", in: &PluginSettings{IndexURL: "file:///tmp/plugin-index"}},
		{name: "bad scheme", in: &PluginSettings{IndexURL: "http://insecure.example.com/idx"}, wantErr: "index_url"},
		{name: "bare path", in: &PluginSettings{IndexURL: "/tmp/plugin-index"}, wantErr: "index_url"},
		{name: "negative ttl", in: &PluginSettings{IndexTTLHours: -1}, wantErr: "index_ttl_hours"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.in.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestPluginSettings_IndexTTL(t *testing.T) {
	t.Parallel()
	var nilSettings *PluginSettings
	if got := nilSettings.IndexTTL(); got != 24*time.Hour {
		t.Errorf("nil receiver TTL = %v, want 24h", got)
	}
	if got := (&PluginSettings{}).IndexTTL(); got != 24*time.Hour {
		t.Errorf("zero TTL = %v, want 24h", got)
	}
	if got := (&PluginSettings{IndexTTLHours: 2}).IndexTTL(); got != 2*time.Hour {
		t.Errorf("TTL = %v, want 2h", got)
	}
}

func TestMergeJSON_PluginsWholeObjectReplace(t *testing.T) {
	t.Parallel()
	base := &EntireSettings{
		Enabled: true,
		Plugins: &PluginSettings{IndexURL: "https://example.com/base", IndexTTLHours: 48},
	}
	// Override sets only index_url; whole-object replacement means the
	// TTL from base is dropped, matching investigate's semantics.
	if err := mergeJSON(base, []byte(`{"plugins":{"index_url":"https://example.com/override"}}`)); err != nil {
		t.Fatalf("mergeJSON: %v", err)
	}
	if base.Plugins.IndexURL != "https://example.com/override" {
		t.Errorf("IndexURL = %q, want override", base.Plugins.IndexURL)
	}
	if base.Plugins.IndexTTLHours != 0 {
		t.Errorf("IndexTTLHours = %d, want 0 (whole-object replace)", base.Plugins.IndexTTLHours)
	}
}

func TestMergeJSON_PluginsAbsentKeyPreservesBase(t *testing.T) {
	t.Parallel()
	base := &EntireSettings{
		Enabled: true,
		Plugins: &PluginSettings{IndexURL: "https://example.com/base"},
	}
	if err := mergeJSON(base, []byte(`{"log_level":"debug"}`)); err != nil {
		t.Fatalf("mergeJSON: %v", err)
	}
	if base.Plugins == nil || base.Plugins.IndexURL != "https://example.com/base" {
		t.Errorf("Plugins = %+v, want base preserved", base.Plugins)
	}
}

func TestEntireSettings_PluginAccessors(t *testing.T) {
	t.Parallel()
	var nilSettings *EntireSettings
	if got := nilSettings.PluginIndexURL(); got != "" {
		t.Errorf("nil PluginIndexURL = %q, want empty", got)
	}
	if got := nilSettings.PluginIndexTTL(); got != 24*time.Hour {
		t.Errorf("nil PluginIndexTTL = %v, want 24h", got)
	}
	s := &EntireSettings{Plugins: &PluginSettings{IndexURL: "https://example.com/idx", IndexTTLHours: 1}}
	if got := s.PluginIndexURL(); got != "https://example.com/idx" {
		t.Errorf("PluginIndexURL = %q", got)
	}
	if got := s.PluginIndexTTL(); got != time.Hour {
		t.Errorf("PluginIndexTTL = %v, want 1h", got)
	}
}
