package cli

import (
	"path/filepath"
	"testing"
)

func TestDefaultTrailWorktreePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		branch      string
		trailNumber int
		want        string
	}{
		{"with number", "peter/feature.auth", 123, filepath.Join("/repo", ".entire", "worktrees", "trail-123-peter-feature.auth")},
		{"without number", "feature/other", 0, filepath.Join("/repo", ".entire", "worktrees", "feature-other")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := defaultTrailWorktreePath("/repo", tt.branch, tt.trailNumber); got != tt.want {
				t.Fatalf("defaultTrailWorktreePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSanitizeTrailWorktreeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{"feature/test", "feature-test"},
		{"Feat_1.2-x", "Feat_1.2-x"},
		{"weird name!", "weird-name"},
		{"---", trailWorktreeFallbackName},
		{"  spaced  ", "spaced"},
	}
	for _, tt := range tests {
		if got := sanitizeTrailWorktreeName(tt.in); got != tt.want {
			t.Fatalf("sanitizeTrailWorktreeName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShellQuotePath(t *testing.T) {
	t.Parallel()

	got := shellQuotePath("/tmp/it's here")
	want := `'/tmp/it'\''s here'`
	if got != want {
		t.Fatalf("shellQuotePath() = %q, want %q", got, want)
	}
}
