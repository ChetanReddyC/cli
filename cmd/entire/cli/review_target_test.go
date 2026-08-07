package cli

import "testing"

func TestNormalizeReviewTargetSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    string
		wantURL bool
		wantErr bool
	}{
		{name: "branch", raw: "feature/review-me", want: "feature/review-me"},
		{name: "trail id", raw: "01JABCDEF", want: "01JABCDEF"},
		{name: "trail URL number", raw: "https://entire.io/gh/entireio/cli/trails/604/review-target", want: "604", wantURL: true},
		{name: "trail URL id", raw: "https://app.entire.io/gh/entireio/cli/trails/01JABCDEF", want: "01JABCDEF", wantURL: true},
		{name: "wrong repo", raw: "https://entire.io/gh/acme/other/trails/7/topic", wantURL: true, wantErr: true},
		{name: "non Entire URL", raw: "https://example.com/gh/entireio/cli/trails/7", wantURL: true, wantErr: true},
		{name: "malformed trail URL", raw: "https://entire.io/gh/entireio/cli/trails", wantURL: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, gotURL, err := normalizeReviewTargetSelector(tt.raw, "gh", "entireio", "cli")
			if (err != nil) != tt.wantErr {
				t.Fatalf("normalizeReviewTargetSelector() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want || gotURL != tt.wantURL {
				t.Fatalf("normalizeReviewTargetSelector() = (%q, %v), want (%q, %v)", got, gotURL, tt.want, tt.wantURL)
			}
		})
	}
}

func TestDefaultReviewWorktreePathDistinguishesLossyBranchNames(t *testing.T) {
	t.Parallel()

	a := defaultReviewWorktreePath("/repo", "feature/x")
	b := defaultReviewWorktreePath("/repo", "feature-x")
	if a == b {
		t.Fatalf("lossy branch names produced the same review worktree path: %s", a)
	}
}
