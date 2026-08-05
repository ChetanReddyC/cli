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

// One definition of an acceptable git URL, shared with the cli package's
// pre-flight check. The two used to disagree on http://, git:// and bare
// absolute paths, so `--index /srv/idx` was accepted while the equivalent
// index_url setting was a hard load failure.
func TestValidateGitURL(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{
		"https://github.com/entireio/plugin-index",
		"ssh://git@github.com/entireio/plugin-index.git",
		"file:///srv/plugin-index",
		"git@github.com:entireio/plugin-index.git",
	} {
		if err := ValidateGitURL(ok); err != nil {
			t.Errorf("ValidateGitURL(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{
		"", "   ", "-x",
		"--upload-pack=touch /tmp/pwned; git-upload-pack",
		"ext::sh -c whoami",
		// Unauthenticated transports: the catalog fetched over them decides
		// what installs with no prompt.
		"http://forge.internal/plugin-index",
		"git://forge.internal/plugin-index",
		// Bare paths: file:// says the same thing unambiguously.
		"/srv/plugin-index",
		"./rel",
		"plugin-index",
		"https://",
	} {
		if err := ValidateGitURL(bad); err == nil {
			t.Errorf("ValidateGitURL(%q) = nil, want error", bad)
		}
	}
}

// index_ttl_hours beyond ~2.5M overflows the nanosecond conversion and wraps
// negative, which reads as "always stale" — the opposite of the very large
// value the user asked for.
func TestPluginSettings_IndexTTLBounds(t *testing.T) {
	t.Parallel()
	if err := (&PluginSettings{IndexTTLHours: maxIndexTTLHours}).Validate(); err != nil {
		t.Errorf("ten years should validate: %v", err)
	}
	for _, h := range []int{maxIndexTTLHours + 1, 2562048, 10000000} {
		p := &PluginSettings{IndexTTLHours: h}
		if err := p.Validate(); err == nil {
			t.Errorf("Validate accepted index_ttl_hours=%d", h)
		}
		// Even reached directly, the window must stay positive.
		if got := p.IndexTTL(); got <= 0 {
			t.Errorf("IndexTTL() for %d = %v, want a positive duration", h, got)
		}
	}
}
