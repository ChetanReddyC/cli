package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"
)

// TargetWorktree describes the checkout prepared for a targeted review.
// Created is false when the branch was already checked out and that existing
// worktree is being reused.
type TargetWorktree struct {
	Path    string
	Created bool
}

type reviewWorktreeRunner func(context.Context, string, []string, io.Reader, io.Writer, io.Writer) error

func runTargetReview(ctx context.Context, cmd *cobra.Command, target string, cleanupWorktree, modeSelected bool, deps Deps) error {
	if modeSelected {
		return errors.New("--target can only be used when running a review")
	}
	if deps.PrepareTarget == nil {
		return errors.New("review target checkout is unavailable")
	}
	prepared, err := deps.PrepareTarget(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), target)
	if err != nil {
		return err
	}
	if err := runReviewInWorktree(ctx, deps.RunInWorktree, prepared.Path, stripReviewTargetArgs(os.Args[1:]), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
		return err
	}
	return finishTargetReview(ctx, cmd, prepared, cleanupWorktree, deps.RemoveTarget)
}

func finishTargetReview(ctx context.Context, cmd *cobra.Command, target TargetWorktree, cleanupWorktree bool, removeTarget func(context.Context, string) error) error {
	out := cmd.OutOrStdout()
	if !target.Created {
		if cleanupWorktree {
			fmt.Fprintf(out, "Kept reused worktree at %s.\n", target.Path)
		}
		return nil
	}

	remove := cleanupWorktree
	if !remove && reviewCommandIsInteractive(cmd) {
		form := newAccessibleForm(huh.NewGroup(
			huh.NewConfirm().
				Title("Remove the temporary review worktree?").
				Description(target.Path).
				Value(&remove),
		))
		if err := form.RunWithContext(ctx); err != nil {
			fmt.Fprintf(out, "Kept worktree at %s.\n", target.Path)
			return err //nolint:wrapcheck // propagate huh cancellation
		}
	}
	if !remove {
		fmt.Fprintf(out, "Kept worktree at %s.\n", target.Path)
		return nil
	}
	if removeTarget == nil {
		return errors.New("review target cleanup is unavailable")
	}
	if err := removeTarget(ctx, target.Path); err != nil {
		return fmt.Errorf("remove review worktree %s: %w", target.Path, err)
	}
	fmt.Fprintf(out, "Removed temporary review worktree %s.\n", target.Path)
	return nil
}

func runReviewInWorktree(ctx context.Context, runner reviewWorktreeRunner, worktreeRoot string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if strings.TrimSpace(worktreeRoot) == "" {
		return errors.New("review target checkout returned an empty worktree path")
	}
	if runner != nil {
		return runner(ctx, worktreeRoot, args, stdin, stdout, stderr)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve entire executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = worktreeRoot
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run review in target worktree: %w", err)
	}
	return nil
}

func stripReviewTargetArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--target" {
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "--target=") || arg == "--cleanup-worktree" || strings.HasPrefix(arg, "--cleanup-worktree=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}
