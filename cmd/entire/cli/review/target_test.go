package review

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestStripReviewTargetArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{name: "separate value", args: []string{"review", "general", "--target", "feature/x", "--base", "main"}, want: []string{"review", "general", "--base", "main"}},
		{name: "equals value", args: []string{"review", "--target=https://entire.io/gh/acme/app/trails/42/topic", "general"}, want: []string{"review", "general"}},
		{name: "cleanup", args: []string{"review", "general", "--target=feature/x", "--cleanup-worktree"}, want: []string{"review", "general"}},
		{name: "cleanup equals", args: []string{"review", "general", "--cleanup-worktree=true", "--target", "feature/x"}, want: []string{"review", "general"}},
		{name: "absent", args: []string{"review", "general"}, want: []string{"review", "general"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := stripReviewTargetArgs(tt.args); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("stripReviewTargetArgs(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestFinishTargetReviewExplicitCleanup(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	removed := ""
	err := finishTargetReview(t.Context(), cmd, TargetWorktree{Path: "/tmp/review", Created: true}, true, func(_ context.Context, path string) error {
		removed = path
		return nil
	})
	if err != nil {
		t.Fatalf("finishTargetReview: %v", err)
	}
	if removed != "/tmp/review" {
		t.Fatalf("removed = %q, want /tmp/review", removed)
	}
	if !strings.Contains(out.String(), "Removed temporary review worktree") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestFinishTargetReviewNeverRemovesReusedWorktree(t *testing.T) {
	t.Parallel()

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	called := false
	err := finishTargetReview(t.Context(), cmd, TargetWorktree{Path: "/tmp/reused", Created: false}, true, func(context.Context, string) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("finishTargetReview: %v", err)
	}
	if called {
		t.Fatal("remove callback called for reused worktree")
	}
}

func TestRunReviewInWorktreeUsesInjectedRunner(t *testing.T) {
	t.Parallel()

	var gotRoot string
	var gotArgs []string
	runner := func(_ context.Context, root string, args []string, _ io.Reader, _, _ io.Writer) error {
		gotRoot = root
		gotArgs = append([]string(nil), args...)
		return nil
	}
	if err := runReviewInWorktree(t.Context(), runner, "/tmp/review", []string{"review", "general"}, bytes.NewReader(nil), io.Discard, io.Discard); err != nil {
		t.Fatalf("runReviewInWorktree: %v", err)
	}
	if gotRoot != "/tmp/review" || !reflect.DeepEqual(gotArgs, []string{"review", "general"}) {
		t.Fatalf("runner got root=%q args=%q", gotRoot, gotArgs)
	}
}
